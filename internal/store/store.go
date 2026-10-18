// Package store is the primary (source-of-truth) relationship tuple store.
//
// Every write gets a monotonically increasing revision and is appended to a
// changelog. The full history is kept so any revision can be read, which is
// what replicas (snapshots at an older revision) and the oracle (ground truth
// at the latest revision) both rely on.
package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Tuple is a relationship: Object#Relation@Subject, e.g.
// doc:42#viewer@user:alice or folder:7#viewer@group:eng#member.
type Tuple struct {
	Object   string `json:"object"`
	Relation string `json:"relation"`
	Subject  string `json:"subject"`
}

func (t Tuple) String() string { return t.Object + "#" + t.Relation + "@" + t.Subject }

// ParseTuple parses "object#relation@subject".
func ParseTuple(s string) (Tuple, error) {
	at := strings.Index(s, "@")
	if at < 0 {
		return Tuple{}, fmt.Errorf("tuple %q: missing @subject", s)
	}
	hash := strings.Index(s[:at], "#")
	if hash < 0 {
		return Tuple{}, fmt.Errorf("tuple %q: missing #relation", s)
	}
	t := Tuple{Object: s[:hash], Relation: s[hash+1 : at], Subject: s[at+1:]}
	return t, t.Validate()
}

func (t Tuple) Validate() error {
	if !strings.Contains(t.Object, ":") || !strings.Contains(t.Subject, ":") {
		return fmt.Errorf("tuple %s: object and subject must be type:id", t)
	}
	switch t.Relation {
	case "owner", "viewer", "parent", "member":
	default:
		return fmt.Errorf("tuple %s: unknown relation %q", t, t.Relation)
	}
	if len(t.String()) > 200 {
		return errors.New("tuple too long")
	}
	return nil
}

type Op string

const (
	OpTouch  Op = "touch"
	OpDelete Op = "delete"
)

// Change is one changelog entry, replicated to every check replica.
type Change struct {
	Rev   int64 `json:"rev"`
	Op    Op    `json:"op"`
	Tuple Tuple `json:"tuple"`
	// At is the simulated time (ms) the primary committed the change.
	At int64 `json:"at"`
	// Revoke marks deletes, which can only shrink access. Revocations also go
	// out on the watermark fast lane.
	Revoke bool `json:"revoke"`
	// Affected lists the objects whose permissions may shrink because of this
	// revocation, including indirect dependents (group -> folder -> doc).
	Affected []string `json:"affected,omitempty"`
}

type version struct {
	rev     int64
	present bool
}

type Store struct {
	rev     int64
	changes []Change
	// "object#relation" -> subject -> presence history (ascending rev)
	index map[string]map[string][]version
}

func New() *Store {
	return &Store{index: map[string]map[string][]version{}}
}

// Rev is the latest committed revision.
func (s *Store) Rev() int64 { return s.rev }

// Changes returns the changelog in revision order. Callers must not modify it.
func (s *Store) Changes() []Change { return s.changes }

// Write commits one change at simulated time `at` and returns the changelog entry.
func (s *Store) Write(op Op, t Tuple, at int64) (Change, error) {
	if err := t.Validate(); err != nil {
		return Change{}, err
	}
	key := t.Object + "#" + t.Relation
	present := s.Has(key, t.Subject, s.rev)
	switch op {
	case OpTouch:
	case OpDelete:
		if !present {
			return Change{}, fmt.Errorf("delete %s: tuple does not exist", t)
		}
	default:
		return Change{}, fmt.Errorf("unknown op %q", op)
	}

	c := Change{Rev: s.rev + 1, Op: op, Tuple: t, At: at}
	if op == OpDelete {
		c.Revoke = true
		// Computed against the state before the delete, while the path still exists.
		c.Affected = s.affectedBy(t)
	}

	s.rev = c.Rev
	subjects := s.index[key]
	if subjects == nil {
		subjects = map[string][]version{}
		s.index[key] = subjects
	}
	subjects[t.Subject] = append(subjects[t.Subject], version{rev: c.Rev, present: op == OpTouch})
	s.changes = append(s.changes, c)
	return c, nil
}

// Has reports whether object#relation@subject exists at revision rev.
func (s *Store) Has(objRel, subject string, rev int64) bool {
	hist := s.index[objRel][subject]
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].rev <= rev {
			return hist[i].present
		}
	}
	return false
}

// Subjects lists the subjects of object#relation at revision rev, sorted.
func (s *Store) Subjects(objRel string, rev int64) []string {
	var out []string
	for subj := range s.index[objRel] {
		if s.Has(objRel, subj, rev) {
			out = append(out, subj)
		}
	}
	sort.Strings(out)
	return out
}

// Tuples lists every tuple present at revision rev, optionally filtered by object.
func (s *Store) Tuples(rev int64, object string) []Tuple {
	var out []Tuple
	for key, subjects := range s.index {
		obj, rel, _ := strings.Cut(key, "#")
		if object != "" && obj != object {
			continue
		}
		for subj := range subjects {
			if s.Has(key, subj, rev) {
				out = append(out, Tuple{Object: obj, Relation: rel, Subject: subj})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// affectedBy computes, at the current revision, every object whose view
// permission could shrink if t were removed.
func (s *Store) affectedBy(t Tuple) []string {
	affected := map[string]bool{}
	if t.Relation == "member" {
		// Walk up nested groups, then collect objects that grant to any of them.
		groups := []string{t.Object}
		seen := map[string]bool{t.Object: true}
		for len(groups) > 0 {
			g := groups[0]
			groups = groups[1:]
			for _, ref := range s.referencing(g + "#member") {
				switch ref.Relation {
				case "member":
					if !seen[ref.Object] {
						seen[ref.Object] = true
						groups = append(groups, ref.Object)
					}
				case "viewer", "owner":
					affected[ref.Object] = true
				}
			}
		}
	} else {
		affected[t.Object] = true
	}

	// Anything inheriting through parent links is affected too.
	queue := make([]string, 0, len(affected))
	for o := range affected {
		queue = append(queue, o)
	}
	for len(queue) > 0 {
		o := queue[0]
		queue = queue[1:]
		for _, ref := range s.referencing(o) {
			if ref.Relation == "parent" && !affected[ref.Object] {
				affected[ref.Object] = true
				queue = append(queue, ref.Object)
			}
		}
	}

	out := make([]string, 0, len(affected))
	for o := range affected {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// referencing returns tuples (at the current revision) whose subject is subj.
func (s *Store) referencing(subj string) []Tuple {
	var out []Tuple
	for key, subjects := range s.index {
		if _, ok := subjects[subj]; ok && s.Has(key, subj, s.rev) {
			obj, rel, _ := strings.Cut(key, "#")
			out = append(out, Tuple{Object: obj, Relation: rel, Subject: subj})
		}
	}
	return out
}
