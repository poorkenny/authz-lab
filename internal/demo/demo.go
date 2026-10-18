// Package demo compares strategies for keeping a permission cache from
// serving stale ALLOWs after a revocation:
//
//	client → Check API → permission cache → primary store (SQLite)
//
// Three strategies:
//
//   - TTL: cache entries live until their TTL expires. Nothing tells the
//     cache about writes.
//   - Invalidation: after each SQLite commit an invalidation is published to
//     the cache. It travels asynchronously and arrives InvalidationDelayMs
//     later, so a check that lands in between still sees the old entry. This
//     window can be shrunk but not removed: the commit and the cache update
//     are two separate events in a distributed system.
//   - Revision-aware: cache entries are stamped with the revision they were
//     computed at, and every check carries the document's revision token (the
//     revision of its last ACL change, like a Zanzibar zookie). An entry older
//     than the token is never served; the check falls back to the primary.
//
// Time is simulated in milliseconds so the race window is reproducible.
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

// Modelled per-hop latencies and timings.
const (
	NetworkMs           = 0.5  // client ↔ Check API, each way
	CacheHitMs          = 0.2  // Check API → cache lookup
	RevCompareMs        = 0.01 // compare entry revision with the token (in memory)
	PrimaryMs           = 15.0 // Check API → SQLite primary: round trip + query
	CacheFillMs         = 0.1  // write the fresh decision back into the cache
	CommitMs            = 15.0 // admin → SQLite primary: transaction commit
	InvalidationDelayMs = 80   // commit → invalidation arrives at the cache
	CacheTTLMs          = 300_000
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
	Rev       int64 `json:"rev"`
	CachedAt  int64 `json:"cachedAt"`
	ExpiresAt int64 `json:"expiresAt"`
}

// InvalidationMsg is a message from the primary to the cache, in flight until DeliverAt.
type InvalidationMsg struct {
	Object    string `json:"object"`
	Rev       int64  `json:"rev"`
	SentAt    int64  `json:"sentAt"`
	DeliverAt int64  `json:"deliverAt"`
}

type Demo struct {
	Strategy Strategy
	Now      int64 // simulated ms
	Store    *store.Store
	Audit    *audit.Log

	cache       map[string]CacheEntry
	pending     []InvalidationMsg
	invalidated map[string]InvalidationMsg // cache key → the invalidation that removed it
	seq         int
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
	d := &Demo{Strategy: strategy, Store: st, Audit: &audit.Log{}, cache: map[string]CacheEntry{}, invalidated: map[string]InvalidationMsg{}}
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

// Advance moves the simulated clock forward.
func (d *Demo) Advance(ms int64) {
	if ms > 0 {
		d.Now += ms
	}
	d.deliver()
}

// deliver applies every invalidation that has arrived by now.
func (d *Demo) deliver() {
	kept := d.pending[:0]
	for _, inv := range d.pending {
		if inv.DeliverAt > d.Now {
			kept = append(kept, inv)
			continue
		}
		for k, e := range d.cache {
			if e.Object == inv.Object {
				delete(d.cache, k)
				d.invalidated[k] = inv
			}
		}
	}
	d.pending = kept
}

// inFlight returns the latest undelivered invalidation for object, if any.
func (d *Demo) inFlight(object string) (InvalidationMsg, bool) {
	var found InvalidationMsg
	ok := false
	for _, inv := range d.pending {
		if inv.Object == object && inv.DeliverAt > d.Now {
			found, ok = inv, true
		}
	}
	return found, ok
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
	T     int64  `json:"t"`
	Op    string `json:"op"`
	Tuple string `json:"tuple"`
	Rev   int64  `json:"rev"`
	// Token is the document's new revision token.
	Token int64  `json:"token"`
	Note  string `json:"note"`
	Hops  []Hop  `json:"hops"`
}

func (d *Demo) Grant(subject, object string) (WriteResult, error) {
	return d.change(store.OpTouch, subject, object)
}

func (d *Demo) Revoke(subject, object string) (WriteResult, error) {
	return d.change(store.OpDelete, subject, object)
}

func (d *Demo) change(op store.Op, subject, object string) (WriteResult, error) {
	if err := validate(subject, object); err != nil {
		return WriteResult{}, err
	}
	d.deliver()
	t := store.Tuple{Object: object, Relation: "viewer", Subject: subject}
	c, err := d.Store.Write(op, t)
	if err != nil {
		return WriteResult{}, err
	}
	d.seq++
	r := WriteResult{T: d.Now, Op: string(op), Tuple: t.String(), Rev: c.Rev, Token: c.Rev}
	r.Hops = append(r.Hops, Hop{From: "admin", To: "primary", Label: fmt.Sprintf("SQLite commit: %s %s → rev %d", op, t, c.Rev), Ms: CommitMs})

	switch d.Strategy {
	case TTL:
		r.Note = fmt.Sprintf("Committed to SQLite at rev %d. Nothing tells the cache: cached decisions for %s stay until their TTL (%d s) expires.", c.Rev, object, CacheTTLMs/1000)
		r.Hops = append(r.Hops, Hop{From: "primary", To: "cache", Label: "no invalidation: entries live until TTL", Kind: "none"})
	case Invalidation:
		inv := InvalidationMsg{Object: object, Rev: c.Rev, SentAt: d.Now, DeliverAt: d.Now + InvalidationDelayMs}
		d.pending = append(d.pending, inv)
		r.Note = fmt.Sprintf("Committed to SQLite at rev %d at t=%d ms. An invalidation for %s was published after the commit; it arrives at the cache at t=%d ms. Until then the cache still holds the old decisions.", c.Rev, d.Now, object, inv.DeliverAt)
		r.Hops = append(r.Hops, Hop{From: "primary", To: "cache", Label: fmt.Sprintf("invalidate %s: published after commit, arrives in %d ms (async)", object, InvalidationDelayMs), Kind: "async"})
	case RevisionAware:
		r.Note = fmt.Sprintf("Committed to SQLite at rev %d. %s's revision token is now %d; requests carry it, so the cache needs no invalidation.", c.Rev, object, c.Rev)
		r.Hops = append(r.Hops, Hop{From: "primary", To: "cache", Label: fmt.Sprintf("no invalidation needed: token for %s is now %d", object, c.Rev), Kind: "none"})
	}
	d.Audit.Append(audit.Record{ID: fmt.Sprintf("w%d", d.seq), T: d.Now, Kind: "write", Tuple: t.String(), Op: string(op), Rev: c.Rev, PrimaryRev: c.Rev, Path: string(d.Strategy)})
	return r, nil
}

type CheckResult struct {
	ID       string   `json:"id"`
	T        int64    `json:"t"`
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
	// RaceWindow is set when an invalidation for this document was still in flight.
	RaceWindow   bool             `json:"raceWindow"`
	Invalidation *InvalidationMsg `json:"invalidation,omitempty"`
	LatencyMs    float64          `json:"latencyMs"`
	Hops         []Hop            `json:"hops"`
	Reason       string           `json:"reason"`
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
	d.deliver()
	d.seq++
	truth, err := d.Store.CanView(subject, object)
	if err != nil {
		return CheckResult{}, err
	}
	r := CheckResult{ID: fmt.Sprintf("c%d", d.seq), T: d.Now, Strategy: d.Strategy, Subject: subject, Object: object, Expected: decision(truth), PrimaryRev: d.Store.Rev()}
	if inv, ok := d.inFlight(object); ok {
		r.RaceWindow, r.Invalidation = true, &inv
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
	expired := hit && entry.ExpiresAt <= d.Now
	if expired {
		delete(d.cache, key)
		hit = false
	}
	if hit {
		r.CacheRev = entry.Rev
	}

	hop := func(h Hop) { r.Hops = append(r.Hops, h); r.LatencyMs += h.Ms }
	hop(Hop{From: "client", To: "checkapi", Label: fmt.Sprintf("can %s view %s?", subject, object), Ms: NetworkMs})
	lookup := "miss"
	switch {
	case hit:
		lookup = fmt.Sprintf("hit: %s @ rev %d", entry.Decision, entry.Rev)
	case expired:
		lookup = "miss (TTL expired)"
	default:
		if inv, ok := d.invalidated[key]; ok {
			lookup = fmt.Sprintf("miss (invalidated at t=%d ms)", inv.DeliverAt)
		}
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
		case r.RaceWindow && entry.Decision != r.Expected:
			r.Reason = fmt.Sprintf("Race window: SQLite committed rev %d at t=%d ms, but its invalidation only reaches the cache at t=%d ms. This check at t=%d ms still hits the old entry (rev %d).", r.Invalidation.Rev, r.Invalidation.SentAt, r.Invalidation.DeliverAt, d.Now, entry.Rev)
		default:
			r.Reason = fmt.Sprintf("Cache hit (computed at rev %d, expires in %d s): served without asking SQLite.", entry.Rev, (entry.ExpiresAt-d.Now)/1000)
		}
	} else {
		r.ServedBy, r.Decision = "primary", decision(truth)
		kind := ""
		switch {
		case hit:
			kind = "fallback"
			r.Reason = fmt.Sprintf("Cache entry rev %d < revision token %d: it predates the last ACL change, so the check fell back to SQLite and refreshed the cache.", entry.Rev, r.RequiredRev)
		case strings.HasPrefix(lookup, "miss (invalidated"):
			r.Reason = "The invalidation already removed the old entry, so the check read SQLite and re-cached the fresh decision."
		default:
			r.Reason = "Cache miss: evaluated in SQLite and cached."
		}
		hop(Hop{From: "checkapi", To: "primary", Label: fmt.Sprintf("SQLite query at rev %d → %s", r.PrimaryRev, r.Decision), Ms: PrimaryMs, Kind: kind})
		hop(Hop{From: "checkapi", To: "cache", Label: fmt.Sprintf("refresh: %s @ rev %d", r.Decision, r.PrimaryRev), Ms: CacheFillMs})
		d.cache[key] = CacheEntry{Subject: subject, Object: object, Decision: r.Decision, Rev: r.PrimaryRev, CachedAt: d.Now, ExpiresAt: d.Now + CacheTTLMs}
		delete(d.invalidated, key)
	}
	r.Correct = r.Decision == r.Expected
	hop(Hop{From: "checkapi", To: "client", Label: r.Decision, Ms: NetworkMs, Kind: map[bool]string{true: "ok", false: "stale"}[r.Correct]})

	d.Audit.Append(audit.Record{
		ID: r.ID, T: d.Now, Kind: "check", Subject: subject, Permission: "view", Object: object,
		Decision: r.Decision, OracleDecision: r.Expected, Stale: r.Decision == "ALLOW" && !truth, FalseDeny: r.Decision == "DENY" && truth,
		ServedBy: r.ServedBy, SnapshotRev: r.CacheRev, RequiredRev: r.RequiredRev, PrimaryRev: r.PrimaryRev,
		Path: string(d.Strategy), LatencyMs: r.LatencyMs, Notes: []string{r.Reason},
	})
	return r, nil
}

type State struct {
	Strategy   Strategy          `json:"strategy"`
	Now        int64             `json:"now"`
	PrimaryRev int64             `json:"primaryRev"`
	Tuples     []string          `json:"tuples"`
	DocRev     map[string]int64  `json:"docRev"`
	Cache      []CacheEntry      `json:"cache"`
	Pending    []InvalidationMsg `json:"pending"`
}

func (d *Demo) State() (State, error) {
	d.deliver()
	s := State{Strategy: d.Strategy, Now: d.Now, PrimaryRev: d.Store.Rev(), Tuples: []string{}, Cache: []CacheEntry{}, Pending: append([]InvalidationMsg{}, d.pending...)}
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
