// Package demo models the smallest system that shows the revocation problem:
//
//	client → Check API → permission cache → primary store
//
// The cache misses invalidations (as a real cache can, while an invalidation
// is lost or still in flight), so after a revocation it keeps serving the old
// ALLOW until its TTL expires.
//
// The fix is a revision-aware fallback: every cache entry is stamped with the
// store revision it was computed at, and every check carries the document's
// revision token (the revision of its last ACL change, stored with the
// document like a Zanzibar zookie). An entry older than the token is never
// served; the check falls back to the primary and refreshes the cache.
package demo

import (
	"fmt"
	"sort"
	"strings"

	"github.com/poorkenny/authz-lab/internal/audit"
	"github.com/poorkenny/authz-lab/internal/engine"
	"github.com/poorkenny/authz-lab/internal/store"
)

type Mode string

const (
	// PlainCache serves any cache hit until its TTL expires.
	PlainCache Mode = "plain"
	// RevisionAware serves a hit only if it is at least as fresh as the
	// document's revision token; otherwise it falls back to the primary.
	RevisionAware Mode = "revision_aware"
)

// Modelled per-hop latencies, used to show what the fallback costs.
const (
	NetworkMs    = 0.5  // client ↔ Check API, each way
	CacheHitMs   = 0.2  // Check API → cache lookup
	RevCompareMs = 0.01 // compare entry revision with the token (in memory)
	PrimaryMs    = 15.0 // Check API → primary store: round trip + evaluation
	CacheFillMs  = 0.1  // write the fresh decision back into the cache
	CacheTTLs    = 300
)

// Hop is one step of a request through the system, with its latency.
type Hop struct {
	From  string  `json:"from"`
	To    string  `json:"to"`
	Label string  `json:"label"`
	Ms    float64 `json:"ms"`
	// Kind is "ok", "stale", "fallback", "lost" or "" for neutral.
	Kind string `json:"kind,omitempty"`
}

func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case PlainCache, RevisionAware:
		return Mode(s), nil
	}
	return "", fmt.Errorf("mode must be %q or %q", PlainCache, RevisionAware)
}

type CacheEntry struct {
	Subject  string `json:"subject"`
	Object   string `json:"object"`
	Decision string `json:"decision"`
	// Rev is the store revision the decision was computed at.
	Rev    int64 `json:"rev"`
	TTLSec int   `json:"ttlSec"`
}

type Demo struct {
	Mode  Mode
	Store *store.Store
	Audit *audit.Log

	cache map[string]CacheEntry
	// docRev is each document's revision token: the revision of its last ACL change.
	docRev map[string]int64
	seq    int
}

// Users and documents the demo knows about.
var (
	Users = []string{"user:alice", "user:bob", "user:carol"}
	Docs  = []string{"doc:42", "doc:7"}
)

// New returns the starting world: Bob owns doc:42 and doc:7, Alice can view doc:42.
func New(mode Mode) *Demo {
	d := &Demo{Mode: mode, Store: store.New(), Audit: &audit.Log{}, cache: map[string]CacheEntry{}, docRev: map[string]int64{}}
	for _, t := range []store.Tuple{
		{Object: "doc:42", Relation: "owner", Subject: "user:bob"},
		{Object: "doc:7", Relation: "owner", Subject: "user:bob"},
		{Object: "doc:42", Relation: "viewer", Subject: "user:alice"},
	} {
		if _, err := d.write(store.OpTouch, t); err != nil {
			panic(err)
		}
	}
	return d
}

func validate(subject, object string) error {
	if !strings.HasPrefix(subject, "user:") || len(subject) > 64 {
		return fmt.Errorf("subject must be user:<id>")
	}
	if !strings.HasPrefix(object, "doc:") || len(object) > 64 {
		return fmt.Errorf("object must be doc:<id>")
	}
	return nil
}

type WriteResult struct {
	Op    string `json:"op"`
	Tuple string `json:"tuple"`
	Rev   int64  `json:"rev"`
	// Token is the document's new revision token.
	Token int64  `json:"token"`
	Note  string `json:"note"`
	Hops  []Hop  `json:"hops"`
}

func (d *Demo) write(op store.Op, t store.Tuple) (store.Change, error) {
	d.seq++
	c, err := d.Store.Write(op, t, int64(d.seq))
	if err != nil {
		return c, err
	}
	d.docRev[t.Object] = c.Rev
	d.Audit.Append(audit.Record{ID: fmt.Sprintf("w%d", d.seq), T: int64(d.seq), Kind: "write", Tuple: t.String(), Op: string(op), Rev: c.Rev, PrimaryRev: c.Rev})
	return c, nil
}

// Grant makes subject a viewer of object.
func (d *Demo) Grant(subject, object string) (WriteResult, error) {
	return d.change(store.OpTouch, subject, object)
}

// Revoke removes subject's viewer access to object.
func (d *Demo) Revoke(subject, object string) (WriteResult, error) {
	return d.change(store.OpDelete, subject, object)
}

func (d *Demo) change(op store.Op, subject, object string) (WriteResult, error) {
	if err := validate(subject, object); err != nil {
		return WriteResult{}, err
	}
	t := store.Tuple{Object: object, Relation: "viewer", Subject: subject}
	c, err := d.write(op, t)
	if err != nil {
		return WriteResult{}, err
	}
	// The cache is deliberately NOT invalidated: this is the one stale cache.
	note := fmt.Sprintf("Committed at rev %d. %s's revision token is now %d. The cache did not receive an invalidation.", c.Rev, object, c.Rev)
	hops := []Hop{
		{From: "admin", To: "primary", Label: fmt.Sprintf("%s %s → commit rev %d", op, t, c.Rev), Ms: PrimaryMs},
		{From: "primary", To: "cache", Label: "invalidation lost: cache keeps its old entry", Kind: "lost"},
	}
	return WriteResult{Op: string(op), Tuple: t.String(), Rev: c.Rev, Token: c.Rev, Note: note, Hops: hops}, nil
}

type CheckResult struct {
	ID       string `json:"id"`
	Mode     Mode   `json:"mode"`
	Subject  string `json:"subject"`
	Object   string `json:"object"`
	Decision string `json:"decision"`
	// Expected is the primary's decision at its latest revision (ground truth).
	Expected string `json:"expected"`
	Correct  bool   `json:"correct"`
	// ServedBy is "cache" or "primary".
	ServedBy string `json:"servedBy"`
	// CacheRev is the revision of the cache entry found (0 if none).
	CacheRev int64 `json:"cacheRev"`
	// RequiredRev is the document's revision token (revision-aware mode only).
	RequiredRev int64 `json:"requiredRev"`
	PrimaryRev  int64 `json:"primaryRev"`
	// LatencyMs is the sum of the hops on the request's critical path.
	LatencyMs float64 `json:"latencyMs"`
	Hops      []Hop   `json:"hops"`
	// Reason explains how the check was served, in one sentence.
	Reason string `json:"reason"`
}

func decision(ok bool) string {
	if ok {
		return "ALLOW"
	}
	return "DENY"
}

func cacheKey(subject, object string) string { return subject + "|view|" + object }

// Check answers "can subject view object?" in the demo's current mode.
func (d *Demo) Check(subject, object string) (CheckResult, error) {
	if err := validate(subject, object); err != nil {
		return CheckResult{}, err
	}
	d.seq++
	primaryRev := d.Store.Rev()
	truth, _, err := engine.Check(d.Store, primaryRev, subject, "view", object)
	if err != nil {
		return CheckResult{}, err
	}
	r := CheckResult{ID: fmt.Sprintf("c%d", d.seq), Mode: d.Mode, Subject: subject, Object: object, Expected: decision(truth), PrimaryRev: primaryRev}

	entry, hit := d.cache[cacheKey(subject, object)]
	if hit {
		r.CacheRev = entry.Rev
	}
	if d.Mode == RevisionAware {
		r.RequiredRev = d.docRev[object]
	}

	hop := func(h Hop) { r.Hops = append(r.Hops, h); r.LatencyMs += h.Ms }
	hop(Hop{From: "client", To: "checkapi", Label: fmt.Sprintf("can %s view %s?", subject, object), Ms: NetworkMs})
	lookup := "cache miss"
	if hit {
		lookup = fmt.Sprintf("hit: %s @ rev %d", entry.Decision, entry.Rev)
	}
	hop(Hop{From: "checkapi", To: "cache", Label: "lookup → " + lookup, Ms: CacheHitMs})

	serveCache := hit && (d.Mode == PlainCache || entry.Rev >= r.RequiredRev)
	if hit && d.Mode == RevisionAware {
		verdict := "fresh"
		if !serveCache {
			verdict = "stale"
		}
		hop(Hop{From: "cache", To: "checkapi", Label: fmt.Sprintf("compare entry rev %d with token %d → %s", entry.Rev, r.RequiredRev, verdict), Ms: RevCompareMs, Kind: map[bool]string{true: "ok", false: "fallback"}[serveCache]})
	}

	if serveCache {
		r.ServedBy, r.Decision = "cache", entry.Decision
		if d.Mode == PlainCache {
			r.Reason = fmt.Sprintf("Cache hit (computed at rev %d, TTL %ds not expired): served without asking the primary.", entry.Rev, entry.TTLSec)
		} else {
			r.Reason = fmt.Sprintf("Cache entry rev %d ≥ revision token %d: it already reflects the latest ACL change, so it is safe to serve.", entry.Rev, r.RequiredRev)
		}
	} else {
		r.ServedBy, r.Decision = "primary", decision(truth)
		if hit {
			r.Reason = fmt.Sprintf("Cache entry rev %d < revision token %d: it predates the last ACL change, so the check fell back to the primary and refreshed the cache.", entry.Rev, r.RequiredRev)
		} else {
			r.Reason = "Cache miss: evaluated at the primary and cached."
		}
		hop(Hop{From: "checkapi", To: "primary", Label: fmt.Sprintf("evaluate at rev %d → %s", primaryRev, r.Decision), Ms: PrimaryMs, Kind: map[bool]string{true: "fallback", false: ""}[hit]})
		hop(Hop{From: "checkapi", To: "cache", Label: fmt.Sprintf("refresh: %s @ rev %d", r.Decision, primaryRev), Ms: CacheFillMs})
		d.cache[cacheKey(subject, object)] = CacheEntry{Subject: subject, Object: object, Decision: r.Decision, Rev: primaryRev, TTLSec: CacheTTLs}
	}
	r.Correct = r.Decision == r.Expected
	hop(Hop{From: "checkapi", To: "client", Label: r.Decision, Ms: NetworkMs, Kind: map[bool]string{true: "ok", false: "stale"}[r.Correct]})

	d.Audit.Append(audit.Record{
		ID: r.ID, T: int64(d.seq), Kind: "check", Subject: subject, Permission: "view", Object: object,
		Decision: r.Decision, OracleDecision: r.Expected, Stale: r.Decision == "ALLOW" && !truth, FalseDeny: r.Decision == "DENY" && truth,
		ServedBy: r.ServedBy, SnapshotRev: r.CacheRev, RequiredRev: r.RequiredRev, PrimaryRev: primaryRev,
		Path: string(d.Mode), LatencyMs: r.LatencyMs, Notes: []string{r.Reason},
	})
	return r, nil
}

type State struct {
	Mode       Mode             `json:"mode"`
	PrimaryRev int64            `json:"primaryRev"`
	Tuples     []string         `json:"tuples"`
	DocRev     map[string]int64 `json:"docRev"`
	Cache      []CacheEntry     `json:"cache"`
}

func (d *Demo) State() State {
	s := State{Mode: d.Mode, PrimaryRev: d.Store.Rev(), DocRev: d.docRev, Cache: []CacheEntry{}, Tuples: []string{}}
	for _, t := range d.Store.Tuples(s.PrimaryRev, "") {
		s.Tuples = append(s.Tuples, t.String())
	}
	for _, e := range d.cache {
		s.Cache = append(s.Cache, e)
	}
	sort.Slice(s.Cache, func(i, j int) bool {
		return cacheKey(s.Cache[i].Subject, s.Cache[i].Object) < cacheKey(s.Cache[j].Subject, s.Cache[j].Object)
	})
	return s
}
