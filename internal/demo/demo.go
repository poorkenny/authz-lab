// Package demo compares strategies for keeping a permission cache from
// serving stale ALLOWs after a revocation:
//
//	client → Check API → permission cache → primary store (SQLite)
//
// Three strategies:
//
//   - TTL: cache entries live until their TTL expires. Nothing tells the
//     cache about writes. With EnforceTTL, expiry runs on a simulated clock
//     that Advance fast-forwards, so the stale window can be shown to end.
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
	"time"

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
	// CachedAt is when the entry was written, on the demo's simulated clock.
	CachedAt time.Time `json:"-"`
	// AgeSec and ExpiresInSec describe the entry's freshness (filled in by State).
	AgeSec       float64 `json:"ageSec"`
	ExpiresInSec float64 `json:"expiresInSec"`
}

// Options are the demo behaviours controlled by feature flags.
type Options struct {
	// AutoDeliver delivers each invalidation event right after its commit
	// instead of waiting for DeliverNext (controlled-cache-delivery flag off).
	AutoDeliver bool
	// EnforceTTL expires cache entries CacheTTLSec after they were cached, on
	// a simulated clock that Advance can fast-forward (strategy-stories flag).
	EnforceTTL bool
	// HappyPath adds, to an invalidation-strategy read made while the user's
	// invalidation is still queued, the result the read would have had if the
	// event had been delivered first (happy-path-comparison flag).
	HappyPath bool
}

type Demo struct {
	Strategy Strategy
	Store    *store.Store
	Audit    *audit.Log
	Queue    *Queue
	Options
	// Now is the wall clock (replaceable in tests); the simulated clock is
	// Now plus the time skipped with Advance.
	Now func() time.Time

	start       time.Time
	skew        time.Duration
	cache       map[string]CacheEntry
	invalidated map[string]QueueMsg // cache key → the delivery that removed it
	expired     map[string]bool     // cache key → the entry was evicted by its TTL
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
	d := &Demo{Strategy: strategy, Store: st, Audit: &audit.Log{}, Queue: &Queue{}, Now: time.Now, start: time.Now(),
		cache: map[string]CacheEntry{}, invalidated: map[string]QueueMsg{}, expired: map[string]bool{}}
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

// clock is the simulated time: wall time plus everything skipped with Advance.
func (d *Demo) clock() time.Time { return d.Now().Add(d.skew) }

// ClockSec is the simulated time in seconds since the world was created.
func (d *Demo) ClockSec() float64 { return d.clock().Sub(d.start).Seconds() }

func (d *Demo) age(e CacheEntry) float64 { return d.clock().Sub(e.CachedAt).Seconds() }

type ClockResult struct {
	AdvancedSec int          `json:"advancedSec"`
	ClockSec    float64      `json:"clockSec"`
	Evicted     []CacheEntry `json:"evicted"`
	Note        string       `json:"note"`
	Hops        []Hop        `json:"hops"`
}

// Advance fast-forwards the simulated clock and evicts every cache entry
// that has reached its TTL.
func (d *Demo) Advance(sec int) ClockResult {
	d.skew += time.Duration(sec) * time.Second
	r := ClockResult{AdvancedSec: sec, ClockSec: d.ClockSec(), Evicted: []CacheEntry{}, Hops: []Hop{}}
	if d.EnforceTTL {
		for key, e := range d.cache {
			if d.age(e) >= CacheTTLSec {
				delete(d.cache, key)
				d.expired[key] = true
				r.Evicted = append(r.Evicted, e)
			}
		}
	}
	sort.Slice(r.Evicted, func(i, j int) bool {
		return cacheKey(r.Evicted[i].Subject, r.Evicted[i].Object) < cacheKey(r.Evicted[j].Subject, r.Evicted[j].Object)
	})
	names := []string{}
	for _, e := range r.Evicted {
		names = append(names, fmt.Sprintf("%s → %s %s @ rev %d", e.Subject, e.Object, e.Decision, e.Rev))
	}
	switch {
	case !d.EnforceTTL:
		r.Note = fmt.Sprintf("Fast-forwarded the clock %d s. TTL expiry is disabled, so nothing was evicted.", sec)
	case len(names) == 0:
		r.Note = fmt.Sprintf("Fast-forwarded the clock %d s. No cache entry has reached its TTL (%d s) yet.", sec, CacheTTLSec)
	default:
		r.Note = fmt.Sprintf("Fast-forwarded the clock %d s. %d cache entr%s reached the TTL (%d s) and %s evicted: %s. The next read goes to SQLite.",
			sec, len(names), map[bool]string{true: "y", false: "ies"}[len(names) == 1], CacheTTLSec, map[bool]string{true: "was", false: "were"}[len(names) == 1], strings.Join(names, ", "))
	}
	return r
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
		r.Note = fmt.Sprintf("Committed to SQLite at rev %d. %s's revision token is now ver#%d; requests carry it, so the cache needs no invalidation.", c.Rev, object, c.Rev)
		r.Hops = append(r.Hops, Hop{From: "primary", To: "queue", Label: fmt.Sprintf("no invalidation needed: %s is now ver#%d", object, c.Rev), Kind: "none"})
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
		return DeliverResult{Note: note, Hops: []Hop{}}
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
	// EntryAgeSec is the age of the cache entry that was found (0 on a miss).
	EntryAgeSec float64 `json:"entryAgeSec"`
	TTLSec      int     `json:"ttlSec"`
	// HappyPath is what this read would have returned had the pending
	// invalidation been delivered first (invalidation strategy, HappyPath on).
	HappyPath *HappyPath `json:"happyPath,omitempty"`
}

// HappyPath is the result of the same check on the happy path: the
// invalidation was delivered before the read.
type HappyPath struct {
	Decision  string  `json:"decision"`
	Correct   bool    `json:"correct"`
	ServedBy  string  `json:"servedBy"`
	LatencyMs float64 `json:"latencyMs"`
	Hops      []Hop   `json:"hops"`
	Note      string  `json:"note"`
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
	r := CheckResult{ID: fmt.Sprintf("c%d", d.seq), Strategy: d.Strategy, Subject: subject, Object: object, Expected: decision(truth), PrimaryRev: d.Store.Rev(), QueueDepth: d.Queue.Depth(), TTLSec: CacheTTLSec}
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
	expiredNow := false
	if hit && d.EnforceTTL && d.age(entry) >= CacheTTLSec {
		// Lazily evict an entry whose TTL ran out since it was cached.
		delete(d.cache, key)
		hit, expiredNow = false, true
	}
	if hit {
		r.CacheRev = entry.Rev
		r.EntryAgeSec = d.age(entry)
	}

	hop := func(h Hop) { r.Hops = append(r.Hops, h); r.LatencyMs += h.Ms }
	hop(Hop{From: "client", To: "checkapi", Label: fmt.Sprintf("can %s view %s?", subject, object), Ms: NetworkMs})
	lookup := "miss"
	if hit {
		lookup = fmt.Sprintf("hit: %s @ rev %d", entry.Decision, entry.Rev)
	} else if m, ok := d.invalidated[key]; ok {
		lookup = fmt.Sprintf("miss (dropped by invalidation #%d)", m.ID)
	} else if expiredNow || d.expired[key] {
		lookup = fmt.Sprintf("miss (expired after the %d s TTL)", CacheTTLSec)
	}
	hop(Hop{From: "checkapi", To: "cache", Label: "lookup → " + lookup, Ms: CacheHitMs})

	serveCache := hit && (d.Strategy != RevisionAware || entry.Rev >= r.RequiredRev)
	if hit && d.Strategy == RevisionAware {
		verdict, kind := "fresh", "ok"
		if !serveCache {
			verdict, kind = "stale", "fallback"
		}
		hop(Hop{From: "cache", To: "checkapi", Label: fmt.Sprintf("compare entry rev %d with %s ver#%d → %s", entry.Rev, object, r.RequiredRev, verdict), Ms: RevCompareMs, Kind: kind})
	}

	if serveCache {
		r.ServedBy, r.Decision = "cache", entry.Decision
		switch {
		case d.Strategy == RevisionAware:
			r.Reason = fmt.Sprintf("Cache entry rev %d ≥ %s ver#%d: it already reflects the latest ACL change, so it is safe to serve.", entry.Rev, object, r.RequiredRev)
		case r.Pending != nil && entry.Decision != r.Expected:
			r.Reason = fmt.Sprintf("Read-after-write inconsistency: SQLite committed rev %d, but invalidation #%d is still queued (position %d of %d), so the cache still serves its old entry (rev %d).", r.Pending.Rev, r.Pending.ID, r.PendingPos, r.QueueDepth, entry.Rev)
		case d.EnforceTTL && entry.Decision != r.Expected:
			r.Reason = fmt.Sprintf("Stale: the entry was cached %.0f s ago at rev %d; SQLite is now at rev %d. Nothing invalidates it, so it is served until its TTL runs out in %.0f s.", r.EntryAgeSec, entry.Rev, r.PrimaryRev, CacheTTLSec-r.EntryAgeSec)
		case d.EnforceTTL:
			r.Reason = fmt.Sprintf("Cache hit: cached %.0f s ago at rev %d, expires in %.0f s (TTL %d s). Served without asking SQLite.", r.EntryAgeSec, entry.Rev, CacheTTLSec-r.EntryAgeSec, CacheTTLSec)
		default:
			r.Reason = fmt.Sprintf("Cache hit (computed at rev %d, TTL %d s): served without asking SQLite.", entry.Rev, CacheTTLSec)
		}
	} else {
		r.ServedBy, r.Decision = "primary", decision(truth)
		kind := ""
		switch {
		case hit:
			kind = "fallback"
			r.Reason = fmt.Sprintf("Cache entry rev %d < %s ver#%d: it predates the last ACL change, so the check fell back to SQLite and refreshed the cache.", entry.Rev, object, r.RequiredRev)
		case strings.HasPrefix(lookup, "miss (dropped"):
			r.Reason = "The delivered invalidation removed the old entry, so the check read SQLite and re-cached the fresh decision."
		case strings.HasPrefix(lookup, "miss (expired"):
			r.Reason = fmt.Sprintf("The old entry reached its TTL (%d s) and was evicted, so the check read SQLite and re-cached the fresh decision.", CacheTTLSec)
		default:
			r.Reason = "Cache miss: evaluated in SQLite and cached."
		}
		hop(Hop{From: "checkapi", To: "primary", Label: fmt.Sprintf("SQLite query at rev %d → %s", r.PrimaryRev, r.Decision), Ms: PrimaryMs, Kind: kind})
		hop(Hop{From: "checkapi", To: "cache", Label: fmt.Sprintf("refresh: %s @ rev %d", r.Decision, r.PrimaryRev), Ms: CacheFillMs})
		d.cache[key] = CacheEntry{Subject: subject, Object: object, Decision: r.Decision, Rev: r.PrimaryRev, CachedAt: d.clock()}
		delete(d.invalidated, key)
		delete(d.expired, key)
	}
	r.Correct = r.Decision == r.Expected
	hop(Hop{From: "checkapi", To: "client", Label: r.Decision, Ms: NetworkMs, Kind: map[bool]string{true: "ok", false: "stale"}[r.Correct]})
	if d.HappyPath && d.Strategy == Invalidation && r.Pending != nil {
		r.HappyPath = happyPath(r)
	}

	d.Audit.Append(audit.Record{
		ID: r.ID, T: int64(d.seq), Kind: "check", Subject: subject, Permission: "view", Object: object,
		Decision: r.Decision, OracleDecision: r.Expected, Stale: r.Decision == "ALLOW" && !truth, FalseDeny: r.Decision == "DENY" && truth,
		ServedBy: r.ServedBy, SnapshotRev: r.CacheRev, RequiredRev: r.RequiredRev, PrimaryRev: r.PrimaryRev,
		Path: string(d.Strategy), LatencyMs: r.LatencyMs, Notes: []string{r.Reason},
	})
	return r, nil
}

// happyPath is the same check had the pending invalidation been delivered
// before the read: the entry is gone, so the lookup misses and SQLite answers.
func happyPath(r CheckResult) *HappyPath {
	hp := &HappyPath{Decision: r.Expected, Correct: true, ServedBy: "primary"}
	for _, h := range []Hop{
		{From: "client", To: "checkapi", Label: fmt.Sprintf("can %s view %s?", r.Subject, r.Object), Ms: NetworkMs},
		{From: "checkapi", To: "cache", Label: fmt.Sprintf("lookup → miss (dropped by invalidation #%d)", r.Pending.ID), Ms: CacheHitMs},
		{From: "checkapi", To: "primary", Label: fmt.Sprintf("SQLite query at rev %d → %s", r.PrimaryRev, r.Expected), Ms: PrimaryMs},
		{From: "checkapi", To: "cache", Label: fmt.Sprintf("refresh: %s @ rev %d", r.Expected, r.PrimaryRev), Ms: CacheFillMs},
		{From: "checkapi", To: "client", Label: r.Expected, Ms: NetworkMs, Kind: "ok"},
	} {
		hp.Hops = append(hp.Hops, h)
		hp.LatencyMs += h.Ms
	}
	if r.Decision == r.Expected {
		hp.Note = fmt.Sprintf("Same decision as the happy path: invalidation #%d is still queued, but this entry already agrees with SQLite.", r.Pending.ID)
	} else {
		hp.Note = fmt.Sprintf("Had invalidation #%d been delivered before this read, the lookup would miss and SQLite would answer %s.", r.Pending.ID, r.Expected)
	}
	return hp
}

type State struct {
	Strategy   Strategy         `json:"strategy"`
	PrimaryRev int64            `json:"primaryRev"`
	Tuples     []string         `json:"tuples"`
	DocRev     map[string]int64 `json:"docRev"`
	Cache      []CacheEntry     `json:"cache"`
	Queued     []QueueMsg       `json:"queued"`
	Delivered  []QueueMsg       `json:"delivered"`
	TTLSec     int              `json:"ttlSec"`
	EnforceTTL bool             `json:"enforceTTL"`
	// ClockSec is the simulated time since the world was created.
	ClockSec float64 `json:"clockSec"`
}

func (d *Demo) State() (State, error) {
	s := State{Strategy: d.Strategy, PrimaryRev: d.Store.Rev(), Tuples: []string{}, Cache: []CacheEntry{}, TTLSec: CacheTTLSec, EnforceTTL: d.EnforceTTL, ClockSec: d.ClockSec()}
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
		e.AgeSec = d.age(e)
		e.ExpiresInSec = max(0, CacheTTLSec-e.AgeSec)
		s.Cache = append(s.Cache, e)
	}
	sort.Slice(s.Cache, func(i, j int) bool {
		return cacheKey(s.Cache[i].Subject, s.Cache[i].Object) < cacheKey(s.Cache[j].Subject, s.Cache[j].Object)
	})
	return s, nil
}
