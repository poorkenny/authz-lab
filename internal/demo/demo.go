// Package demo compares strategies for keeping a permission cache from
// serving stale ALLOWs after a revocation:
//
//	client → Check API → permission cache → primary store (SQLite)
//
// Three strategies:
//
//   - TTL: cache entries live until their TTL expires. Nothing tells the
//     cache about writes.
//   - Invalidation: after each SQLite commit an invalidation event is put on
//     a delivery queue. Until it is delivered the cache still holds the old
//     entry, so a read right after the write can return stale data. Delivery
//     is triggered explicitly (DeliverNext), so runs are deterministic.
//   - Revision-aware: cache entries are stamped with the revision they were
//     computed at, and every check carries the document's revision token (the
//     revision of its last ACL change, like a Zanzibar zookie). An entry older
//     than the token is never served; the check falls back to the primary.
package demo

import (
	"fmt"
	"sort"
	"strings"

	"github.com/poorkenny/authz-lab/internal/audit"
	"github.com/poorkenny/authz-lab/internal/store"
)

type Strategy string

const (
	TTL           Strategy = "ttl"
	Invalidation  Strategy = "invalidation"
	RevisionAware Strategy = "revision_aware"
)

var Strategies = []Strategy{TTL, Invalidation, RevisionAware}

func ParseStrategy(s string) (Strategy, error) {
	for _, st := range Strategies {
		if string(st) == s {
			return st, nil
		}
	}
	return "", fmt.Errorf("strategy must be one of %q, %q, %q", TTL, Invalidation, RevisionAware)
}

// Modelled per-hop latencies.
const (
	NetworkMs    = 0.5  // client ↔ Check API, each way
	CacheHitMs   = 0.2  // Check API → cache lookup
	RevCompareMs = 0.01 // compare entry revision with the token (in memory)
	PrimaryMs    = 15.0 // Check API → SQLite primary: round trip + query
	CacheFillMs  = 0.1  // write the fresh decision back into the cache
	CommitMs     = 15.0 // admin → SQLite primary: transaction commit
	EnqueueMs    = 0.3  // publish an invalidation event onto the queue
	CacheTTLSec  = 300
)

// Hop is one step of a request through the system, with its latency.
type Hop struct {
	From  string  `json:"from"`
	To    string  `json:"to"`
	Label string  `json:"label"`
	Ms    float64 `json:"ms"`
	// Kind is "ok", "stale", "fallback", "async", "none" or "" for neutral.
	Kind string `json:"kind,omitempty"`
}

type CacheEntry struct {
	Subject  string `json:"subject"`
	Object   string `json:"object"`
	Decision string `json:"decision"`
	// Rev is the store revision the decision was computed at.
	Rev int64 `json:"rev"`
}

type Demo struct {
	Strategy Strategy
	Store    *store.Store
	Audit    *audit.Log
	Queue    *Queue
	// AutoDeliver delivers each invalidation event right after its commit
	// instead of waiting for DeliverNext (controlled-cache-delivery flag off).
	AutoDeliver bool

	cache       map[string]CacheEntry
	invalidated map[string]QueueMsg // cache key → the delivery that removed it
	seq         int
	others      int // unrelated writes created by AddOtherWrites
}

var (
	Users = []string{"user:alice", "user:bob", "user:carol"}
	Docs  = []string{"doc:42", "doc:7"}
)

// New returns the starting world: Bob owns doc:42 and doc:7, Alice can view doc:42.
func New(strategy Strategy) (*Demo, error) {
	st, err := store.Open()
	if err != nil {
		return nil, err
	}
	d := &Demo{Strategy: strategy, Store: st, Audit: &audit.Log{}, Queue: &Queue{}, cache: map[string]CacheEntry{}, invalidated: map[string]QueueMsg{}}
	for _, t := range []store.Tuple{
		{Object: "doc:42", Relation: "owner", Subject: "user:bob"},
		{Object: "doc:7", Relation: "owner", Subject: "user:bob"},
		{Object: "doc:42", Relation: "viewer", Subject: "user:alice"},
	} {
		if _, err := st.Write(store.OpTouch, t); err != nil {
			st.Close()
			return nil, err
		}
	}
	return d, nil
}

func (d *Demo) Close() error { return d.Store.Close() }

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
	// Message is the queued invalidation event (invalidation strategy only).
	Message *QueueMsg `json:"message,omitempty"`
}

func (d *Demo) Grant(subject, object string) (WriteResult, error) {
	return d.change(store.OpTouch, subject, object)
}

func (d *Demo) Revoke(subject, object string) (WriteResult, error) {
	return d.change(store.OpDelete, subject, object)
}

// AddOtherWrites commits n unrelated permission changes (new viewers on
// doc:7), each of which queues its own invalidation event.
func (d *Demo) AddOtherWrites(n int) ([]WriteResult, error) {
	var out []WriteResult
	for range n {
		d.others++
		r, err := d.Grant(fmt.Sprintf("user:u%d", d.others), "doc:7")
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (d *Demo) change(op store.Op, subject, object string) (WriteResult, error) {
	if err := validate(subject, object); err != nil {
		return WriteResult{}, err
	}
	t := store.Tuple{Object: object, Relation: "viewer", Subject: subject}
	c, err := d.Store.Write(op, t)
	if err != nil {
		return WriteResult{}, err
	}
	d.seq++
	r := WriteResult{Op: string(op), Tuple: t.String(), Rev: c.Rev, Token: c.Rev}
	r.Hops = append(r.Hops, Hop{From: "admin", To: "primary", Label: fmt.Sprintf("SQLite commit: %s %s → rev %d", op, t, c.Rev), Ms: CommitMs})

	switch d.Strategy {
	case TTL:
		r.Note = fmt.Sprintf("Committed to SQLite at rev %d. Nothing tells the cache: cached decisions for %s stay until their TTL (%d s) expires.", c.Rev, subject, CacheTTLSec)
		r.Hops = append(r.Hops, Hop{From: "primary", To: "queue", Label: "no invalidation: entries live until TTL", Kind: "none"})
	case Invalidation:
		m := d.Queue.Enqueue(subject, object, string(op), c.Rev)
		r.Message = &m
		r.Note = fmt.Sprintf("Committed to SQLite at rev %d. Invalidation #%d for %s → %s is now queued (position %d of %d). Until it is delivered, the cache still holds the old decision.", c.Rev, m.ID, subject, object, d.Queue.Depth(), d.Queue.Depth())
		r.Hops = append(r.Hops, Hop{From: "primary", To: "queue", Label: fmt.Sprintf("enqueue invalidation #%d (queue depth %d)", m.ID, d.Queue.Depth()), Ms: EnqueueMs, Kind: "async"})
		if d.AutoDeliver {
			dr := d.DeliverNext()
			r.Hops = append(r.Hops, dr.Hops...)
			r.Note = fmt.Sprintf("Committed to SQLite at rev %d. Invalidation #%d was delivered automatically right after the commit: %s", c.Rev, m.ID, dr.Note)
		}
	case RevisionAware:
		r.Note = fmt.Sprintf("Committed to SQLite at rev %d. %s's revision token is now %d; requests carry it, so the cache needs no invalidation.", c.Rev, object, c.Rev)
		r.Hops = append(r.Hops, Hop{From: "primary", To: "queue", Label: fmt.Sprintf("no invalidation needed: token for %s is now %d", object, c.Rev), Kind: "none"})
	}
	d.Audit.Append(audit.Record{ID: fmt.Sprintf("w%d", d.seq), T: int64(d.seq), Kind: "write", Tuple: t.String(), Op: string(op), Rev: c.Rev, PrimaryRev: c.Rev, Path: string(d.Strategy)})
	return r, nil
}

type DeliverResult struct {
	Delivered bool      `json:"delivered"`
	Message   *QueueMsg `json:"message,omitempty"`
	// Removed reports whether a cache entry was dropped by this delivery.
	Removed bool   `json:"removed"`
	Note    string `json:"note"`
	Hops    []Hop  `json:"hops"`
	Depth   int    `json:"depth"`
}

// DeliverNext delivers the invalidation event at the head of the queue.
func (d *Demo) DeliverNext() DeliverResult {
	m, ok := d.Queue.DeliverNext()
	if !ok {
		note := "The queue is empty: nothing to deliver."
		switch d.Strategy {
		case TTL:
			note = "The TTL strategy sends no invalidations, so there is nothing to deliver. The cache keeps its entries until they expire."
		case RevisionAware:
			note = "The revision-aware strategy needs no invalidations, so there is nothing to deliver."
		}
		return DeliverResult{Note: note}
	}
	key := cacheKey(m.Subject, m.Object)
	_, removed := d.cache[key]
	if removed {
		delete(d.cache, key)
		d.invalidated[key] = m
	}
	r := DeliverResult{Delivered: true, Message: &m, Removed: removed, Depth: d.Queue.Depth()}
	what := "no cached entry to drop"
	if removed {
		what = fmt.Sprintf("dropped the cached decision for %s → %s", m.Subject, m.Object)
	}
	r.Note = fmt.Sprintf("Delivered invalidation #%d: %s. %d still queued.", m.ID, what, r.Depth)
	r.Hops = []Hop{{From: "queue", To: "cache", Label: fmt.Sprintf("deliver #%d: %s", m.ID, what), Kind: "async"}}
	return r
}

type CheckResult struct {
	ID       string   `json:"id"`
	Strategy Strategy `json:"strategy"`
	Subject  string   `json:"subject"`
	Object   string   `json:"object"`
	Decision string   `json:"decision"`
	// Expected is SQLite's answer at the moment of the check (ground truth).
	Expected string `json:"expected"`
	Correct  bool   `json:"correct"`
	ServedBy string `json:"servedBy"` // cache | primary
	CacheRev int64  `json:"cacheRev"`
	// RequiredRev is the document's revision token (revision-aware only).
	RequiredRev int64 `json:"requiredRev"`
	PrimaryRev  int64 `json:"primaryRev"`
	// Pending is the undelivered invalidation for this user/document, if any,
	// with its 1-based position in the queue.
	Pending    *QueueMsg `json:"pending,omitempty"`
	PendingPos int       `json:"pendingPos,omitempty"`
	QueueDepth int       `json:"queueDepth"`
	LatencyMs  float64   `json:"latencyMs"`
	Hops       []Hop     `json:"hops"`
	Reason     string    `json:"reason"`
}

func decision(ok bool) string {
	if ok {
		return "ALLOW"
	}
	return "DENY"
}

func cacheKey(subject, object string) string { return subject + "|view|" + object }

// Check answers "can subject view object?" with the demo's current strategy.
func (d *Demo) Check(subject, object string) (CheckResult, error) {
	if err := validate(subject, object); err != nil {
		return CheckResult{}, err
	}
	d.seq++
	truth, err := d.Store.CanView(subject, object)
	if err != nil {
		return CheckResult{}, err
	}
	r := CheckResult{ID: fmt.Sprintf("c%d", d.seq), Strategy: d.Strategy, Subject: subject, Object: object, Expected: decision(truth), PrimaryRev: d.Store.Rev(), QueueDepth: d.Queue.Depth()}
	if m, pos, ok := d.Queue.Pending(subject, object); ok {
		r.Pending, r.PendingPos = &m, pos
	}
	if d.Strategy == RevisionAware {
		// In a real system the token travels with the request (the app read it
		// along with the document); the simulation looks it up directly, off
		// the latency path.
		if r.RequiredRev, err = d.Store.DocRev(object); err != nil {
			return CheckResult{}, err
		}
	}

	key := cacheKey(subject, object)
	entry, hit := d.cache[key]
	if hit {
		r.CacheRev = entry.Rev
	}

	hop := func(h Hop) { r.Hops = append(r.Hops, h); r.LatencyMs += h.Ms }
	hop(Hop{From: "client", To: "checkapi", Label: fmt.Sprintf("can %s view %s?", subject, object), Ms: NetworkMs})
	lookup := "miss"
	if hit {
		lookup = fmt.Sprintf("hit: %s @ rev %d", entry.Decision, entry.Rev)
	} else if m, ok := d.invalidated[key]; ok {
		lookup = fmt.Sprintf("miss (dropped by invalidation #%d)", m.ID)
	}
	hop(Hop{From: "checkapi", To: "cache", Label: "lookup → " + lookup, Ms: CacheHitMs})

	serveCache := hit && (d.Strategy != RevisionAware || entry.Rev >= r.RequiredRev)
	if hit && d.Strategy == RevisionAware {
		verdict, kind := "fresh", "ok"
		if !serveCache {
			verdict, kind = "stale", "fallback"
		}
		hop(Hop{From: "cache", To: "checkapi", Label: fmt.Sprintf("compare entry rev %d with token %d → %s", entry.Rev, r.RequiredRev, verdict), Ms: RevCompareMs, Kind: kind})
	}

	if serveCache {
		r.ServedBy, r.Decision = "cache", entry.Decision
		switch {
		case d.Strategy == RevisionAware:
			r.Reason = fmt.Sprintf("Cache entry rev %d ≥ revision token %d: it already reflects the latest ACL change, so it is safe to serve.", entry.Rev, r.RequiredRev)
		case r.Pending != nil && entry.Decision != r.Expected:
			r.Reason = fmt.Sprintf("Read-after-write inconsistency: SQLite committed rev %d, but invalidation #%d is still queued (position %d of %d), so the cache still serves its old entry (rev %d).", r.Pending.Rev, r.Pending.ID, r.PendingPos, r.QueueDepth, entry.Rev)
		default:
			r.Reason = fmt.Sprintf("Cache hit (computed at rev %d, TTL %d s): served without asking SQLite.", entry.Rev, CacheTTLSec)
		}
	} else {
		r.ServedBy, r.Decision = "primary", decision(truth)
		kind := ""
		switch {
		case hit:
			kind = "fallback"
			r.Reason = fmt.Sprintf("Cache entry rev %d < revision token %d: it predates the last ACL change, so the check fell back to SQLite and refreshed the cache.", entry.Rev, r.RequiredRev)
		case strings.HasPrefix(lookup, "miss (dropped"):
			r.Reason = "The delivered invalidation removed the old entry, so the check read SQLite and re-cached the fresh decision."
		default:
			r.Reason = "Cache miss: evaluated in SQLite and cached."
		}
		hop(Hop{From: "checkapi", To: "primary", Label: fmt.Sprintf("SQLite query at rev %d → %s", r.PrimaryRev, r.Decision), Ms: PrimaryMs, Kind: kind})
		hop(Hop{From: "checkapi", To: "cache", Label: fmt.Sprintf("refresh: %s @ rev %d", r.Decision, r.PrimaryRev), Ms: CacheFillMs})
		d.cache[key] = CacheEntry{Subject: subject, Object: object, Decision: r.Decision, Rev: r.PrimaryRev}
		delete(d.invalidated, key)
	}
	r.Correct = r.Decision == r.Expected
	hop(Hop{From: "checkapi", To: "client", Label: r.Decision, Ms: NetworkMs, Kind: map[bool]string{true: "ok", false: "stale"}[r.Correct]})

	d.Audit.Append(audit.Record{
		ID: r.ID, T: int64(d.seq), Kind: "check", Subject: subject, Permission: "view", Object: object,
		Decision: r.Decision, OracleDecision: r.Expected, Stale: r.Decision == "ALLOW" && !truth, FalseDeny: r.Decision == "DENY" && truth,
		ServedBy: r.ServedBy, SnapshotRev: r.CacheRev, RequiredRev: r.RequiredRev, PrimaryRev: r.PrimaryRev,
		Path: string(d.Strategy), LatencyMs: r.LatencyMs, Notes: []string{r.Reason},
	})
	return r, nil
}

type State struct {
	Strategy   Strategy         `json:"strategy"`
	PrimaryRev int64            `json:"primaryRev"`
	Tuples     []string         `json:"tuples"`
	DocRev     map[string]int64 `json:"docRev"`
	Cache      []CacheEntry     `json:"cache"`
	Queued     []QueueMsg       `json:"queued"`
	Delivered  []QueueMsg       `json:"delivered"`
}

func (d *Demo) State() (State, error) {
	s := State{Strategy: d.Strategy, PrimaryRev: d.Store.Rev(), Tuples: []string{}, Cache: []CacheEntry{}}
	s.Delivered, s.Queued = d.Queue.Snapshot()
	tuples, err := d.Store.Tuples()
	if err != nil {
		return s, err
	}
	for _, t := range tuples {
		s.Tuples = append(s.Tuples, t.String())
	}
	if s.DocRev, err = d.Store.DocRevs(); err != nil {
		return s, err
	}
	for _, e := range d.cache {
		s.Cache = append(s.Cache, e)
	}
	sort.Slice(s.Cache, func(i, j int) bool {
		return cacheKey(s.Cache[i].Subject, s.Cache[i].Object) < cacheKey(s.Cache[j].Subject, s.Cache[j].Object)
	})
	return s, nil
}
