// Package engine evaluates permission checks against a store snapshot.
//
// Schema (fixed for the lab):
//
//	group:  member = user | group#member
//	folder: view   = viewer + owner + parent->view
//	doc:    view   = viewer + owner + parent->view
//
// viewer/owner subjects may be a user or group:<id>#member.
package engine

import (
	"fmt"
	"strings"

	"github.com/poorkenny/authz-lab/internal/store"
)

const maxDepth = 8

// Schema is the human-readable schema the UI shows.
const Schema = `definition user {}

definition group {
  relation member: user | group#member
}

definition folder {
  relation owner:  user | group#member
  relation viewer: user | group#member
  relation parent: folder
  permission view = viewer + owner + parent->view
}

definition doc {
  relation owner:  user | group#member
  relation viewer: user | group#member
  relation parent: folder
  permission view = viewer + owner + parent->view
}`

// Check evaluates `user` has `perm` on `object` at revision rev. It returns the
// decision and, when allowed, the resolution path from the user to the object.
func Check(s *store.Store, rev int64, user, perm, object string) (bool, []string, error) {
	if perm != "view" {
		return false, nil, fmt.Errorf("unknown permission %q (only \"view\" is defined)", perm)
	}
	if !strings.HasPrefix(user, "user:") {
		return false, nil, fmt.Errorf("subject %q must be a user", user)
	}
	e := &evaluator{s: s, rev: rev, user: user}
	ok := e.view(object, 0)
	return ok, e.trace, nil
}

type evaluator struct {
	s     *store.Store
	rev   int64
	user  string
	trace []string
}

func (e *evaluator) view(obj string, depth int) bool {
	if depth > maxDepth {
		return false
	}
	for _, rel := range []string{"owner", "viewer"} {
		for _, subj := range e.s.Subjects(obj+"#"+rel, e.rev) {
			if subj == e.user || (strings.HasSuffix(subj, "#member") && e.member(strings.TrimSuffix(subj, "#member"), depth+1)) {
				e.trace = append(e.trace, obj+"#"+rel+"@"+subj)
				return true
			}
		}
	}
	for _, parent := range e.s.Subjects(obj+"#parent", e.rev) {
		if e.view(parent, depth+1) {
			e.trace = append(e.trace, obj+"#parent@"+parent)
			return true
		}
	}
	return false
}

func (e *evaluator) member(group string, depth int) bool {
	if depth > maxDepth {
		return false
	}
	for _, subj := range e.s.Subjects(group+"#member", e.rev) {
		if subj == e.user || (strings.HasSuffix(subj, "#member") && e.member(strings.TrimSuffix(subj, "#member"), depth+1)) {
			e.trace = append(e.trace, group+"#member@"+subj)
			return true
		}
	}
	return false
}
