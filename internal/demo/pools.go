package demo

import (
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/poorkenny/authz-lab/internal/store"
)

// Pools simulates the invalidation strategy with several service instances,
// each publishing to its own invalidation pool (FIFO queue) with its own
// worker. Events in one pool are delivered in order, but nothing orders
// events across pools, so two events for the same entry can be applied in
// the opposite order to their commits.
//
// How harmful that is depends on what applying an event does:
//
//   - PoolRetire: the event deletes the cached entry. Deletes commute, so
//     the order doesn't matter; the next read goes to SQLite.
//   - PoolRefresh: the event writes the new decision and its revision into
//     the cache. A late, older event overwrites a newer entry, and since no
//     further event is coming the stale entry stays until its TTL.
//   - PoolFenced: refresh with a revision fence. The write is a
//     compare-and-set on the entry's revision, so an older revision never
//     overwrites a newer one; the late event is discarded.
//
// A worker can also pick up an event and stall before writing it (Pick),
// so two workers race on the same entry even when they picked their events
// in commit order. The revision fence and PoolByDocument (all events for a
// document go to one pool) both prevent the stale overwrite.
type Pools struct {
	Store   *store.Store
	Mode    string // PoolRetire | PoolRefresh | PoolFenced
	Routing string // PoolByInstance | PoolByDocument

	queues   [NumPools][]PoolEvent
	inflight [NumPools]*PoolEvent
	cache    map[string]CacheEntry
	history  []PoolApply // recent applies, newest first
	nextID   int
	others   int
}

const (
	NumPools       = 2
	PoolRetire     = "retire"
	PoolRefresh    = "refresh"
	PoolFenced     = "fenced"
	PoolByInstance = "instance"
	PoolByDocument = "document"
	maxPoolEvents  = 30
	maxPoolApplies = 10
)

// PoolNames labels the instances and their pools in notes.
var PoolNames = [NumPools]string{"A", "B"}

// PoolEvent is one invalidation event. It carries the decision and revision
// of its commit, which a refresh writes into the cache.
type PoolEvent struct {
	ID       int    `json:"id"`
	Pool     int    `json:"pool"`
	Subject  string `json:"subject"`
	Object   string `json:"object"`
	Op       string `json:"op"`
	Rev      int64  `json:"rev"`
	Decision string `json:"decision"`
}

// PoolApply records what a worker did with an event.
type PoolApply struct {
	Event PoolEvent `json:"event"`
	// Result is "retired", "refreshed" or "discarded" (by the revision fence).
	Result string `json:"result"`
	// Stale: a refresh overwrote a newer entry with an older revision.
	Stale bool   `json:"stale"`
	Note  string `json:"note"`
}

// NewPools returns the starting world (as New) with empty pools.
func NewPools(mode, routing string) (*Pools, error) {
	if mode != PoolRetire && mode != PoolRefresh && mode != PoolFenced {
		return nil, fmt.Errorf("mode must be %q, %q or %q", PoolRetire, PoolRefresh, PoolFenced)
	}
	if routing != PoolByInstance && routing != PoolByDocument {
		return nil, fmt.Errorf("routing must be %q or %q", PoolByInstance, PoolByDocument)
	}
	st, err := store.Open()
	if err != nil {
		return nil, err
	}
	if err := seed(st); err != nil {
		st.Close()
		return nil, err
	}
	return &Pools{Store: st, Mode: mode, Routing: routing, cache: map[string]CacheEntry{}}, nil
}

func (p *Pools) Close() error { return p.Store.Close() }

// DocPool is the pool every event for object goes to with PoolByDocument.
func DocPool(object string) int {
	h := fnv.New32a()
	h.Write([]byte(object))
	return int(h.Sum32() % NumPools)
}

func validPool(i int) error {
	if i < 0 || i >= NumPools {
		return fmt.Errorf("pool/instance must be 0 (A) or 1 (B)")
	}
	return nil
}

func (p *Pools) depth() int {
	n := 0
	for _, q := range p.queues {
		n += len(q)
	}
	return n
}

type PoolWriteResult struct {
	Op    string    `json:"op"`
	Tuple string    `json:"tuple"`
	Rev   int64     `json:"rev"`
	Event PoolEvent `json:"event"`
	Note  string    `json:"note"`
}

// Write commits a grant or revoke through the given service instance and
// queues its invalidation event on that instance's pool (PoolByInstance) or
// on the document's pool (PoolByDocument).
func (p *Pools) Write(op store.Op, subject, object string, instance int) (PoolWriteResult, error) {
	if err := validate(subject, object); err != nil {
		return PoolWriteResult{}, err
	}
	if err := validPool(instance); err != nil {
		return PoolWriteResult{}, err
	}
	if p.depth() >= maxPoolEvents {
		return PoolWriteResult{}, fmt.Errorf("the pools hold at most %d events; deliver some first", maxPoolEvents)
	}
	t := store.Tuple{Object: object, Relation: "viewer", Subject: subject}
	c, err := p.Store.Write(op, t)
	if err != nil {
		return PoolWriteResult{}, err
	}
	allowed, err := p.Store.CanView(subject, object)
	if err != nil {
		return PoolWriteResult{}, err
	}
	pool, how := instance, fmt.Sprintf("instance %s publishes to its own pool %s", PoolNames[instance], PoolNames[instance])
	if p.Routing == PoolByDocument {
		pool = DocPool(object)
		how = fmt.Sprintf("events for %s always go to pool %s", object, PoolNames[pool])
	}
	p.nextID++
	e := PoolEvent{ID: p.nextID, Pool: pool, Subject: subject, Object: object, Op: string(op), Rev: c.Rev, Decision: decision(allowed)}
	p.queues[pool] = append(p.queues[pool], e)
	return PoolWriteResult{Op: string(op), Tuple: t.String(), Rev: c.Rev, Event: e,
		Note: fmt.Sprintf("Instance %s committed %s %s at rev %d. Event #%d (%s @ rev %d) is queued on pool %s (%s), position %d.",
			PoolNames[instance], op, t, c.Rev, e.ID, e.Decision, e.Rev, PoolNames[pool], how, len(p.queues[pool]))}, nil
}

// AddOtherWrites commits n unrelated grants (new viewers on doc:42) through
// the given instance, to back its pool up.
func (p *Pools) AddOtherWrites(n, instance int) ([]PoolWriteResult, error) {
	var out []PoolWriteResult
	for range n {
		p.others++
		r, err := p.Write(store.OpTouch, fmt.Sprintf("user:u%d", p.others), "doc:42", instance)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

type PoolDeliverResult struct {
	Delivered bool       `json:"delivered"`
	Apply     *PoolApply `json:"apply,omitempty"`
	Note      string     `json:"note"`
}

// Deliver lets a pool's worker apply its next event: the one it stalled on
// (see Pick), otherwise the head of its queue.
func (p *Pools) Deliver(pool int) (PoolDeliverResult, error) {
	if err := validPool(pool); err != nil {
		return PoolDeliverResult{}, err
	}
	e := p.inflight[pool]
	resumed := e != nil
	if resumed {
		p.inflight[pool] = nil
	} else {
		if len(p.queues[pool]) == 0 {
			return PoolDeliverResult{Note: fmt.Sprintf("Pool %s is empty: nothing to deliver.", PoolNames[pool])}, nil
		}
		e = &p.queues[pool][0]
		p.queues[pool] = p.queues[pool][1:]
	}
	a := p.apply(*e)
	if resumed {
		a.Note = fmt.Sprintf("Worker %s woke up. %s", PoolNames[pool], a.Note)
	}
	return PoolDeliverResult{Delivered: true, Apply: &a, Note: a.Note}, nil
}

// Pick makes a pool's worker take the head of its queue and stall (a GC
// pause, a slow network) before applying it, until Deliver resumes it.
func (p *Pools) Pick(pool int) (PoolDeliverResult, error) {
	if err := validPool(pool); err != nil {
		return PoolDeliverResult{}, err
	}
	if e := p.inflight[pool]; e != nil {
		return PoolDeliverResult{}, fmt.Errorf("worker %s is already stalled on event #%d; resume it first", PoolNames[pool], e.ID)
	}
	if len(p.queues[pool]) == 0 {
		return PoolDeliverResult{Note: fmt.Sprintf("Pool %s is empty: nothing to pick up.", PoolNames[pool])}, nil
	}
	e := p.queues[pool][0]
	p.queues[pool] = p.queues[pool][1:]
	p.inflight[pool] = &e
	verb := "retire"
	if p.Mode != PoolRetire {
		verb = fmt.Sprintf("write %s @ rev %d into", e.Decision, e.Rev)
	}
	return PoolDeliverResult{Note: fmt.Sprintf("Worker %s took event #%d and stalled before it could %s the cache entry for %s → %s. Other pools keep going.",
		PoolNames[pool], e.ID, verb, e.Subject, e.Object)}, nil
}

func (p *Pools) apply(e PoolEvent) PoolApply {
	key := cacheKey(e.Subject, e.Object)
	cur, ok := p.cache[key]
	a := PoolApply{Event: e}
	switch {
	case p.Mode == PoolRetire:
		delete(p.cache, key)
		a.Result = "retired"
		if ok {
			a.Note = fmt.Sprintf("Event #%d retired the cached entry for %s → %s (%s @ rev %d). The next read goes to SQLite, whatever order the events arrive in.", e.ID, e.Subject, e.Object, cur.Decision, cur.Rev)
		} else {
			a.Note = fmt.Sprintf("Event #%d: nothing cached for %s → %s, nothing to retire.", e.ID, e.Subject, e.Object)
		}
	case ok && cur.Rev > e.Rev && p.Mode == PoolFenced:
		a.Result = "discarded"
		a.Note = fmt.Sprintf("Event #%d (%s @ rev %d) arrived after the cache already holds %s @ rev %d. The revision fence discards it: an older revision never overwrites a newer one.", e.ID, e.Decision, e.Rev, cur.Decision, cur.Rev)
	default:
		p.cache[key] = CacheEntry{Subject: e.Subject, Object: e.Object, Decision: e.Decision, Rev: e.Rev}
		a.Result = "refreshed"
		switch {
		case ok && cur.Rev > e.Rev:
			a.Stale = true
			a.Note = fmt.Sprintf("Event #%d (%s @ rev %d) overwrote the newer entry %s @ rev %d. The cache now serves an older revision, and no event is coming to fix it: it stays until its TTL (%d s).", e.ID, e.Decision, e.Rev, cur.Decision, cur.Rev, CacheTTLSec)
		case ok:
			a.Note = fmt.Sprintf("Event #%d refreshed %s → %s from %s @ rev %d to %s @ rev %d.", e.ID, e.Subject, e.Object, cur.Decision, cur.Rev, e.Decision, e.Rev)
		default:
			a.Note = fmt.Sprintf("Event #%d refreshed %s → %s: the cache now holds %s @ rev %d.", e.ID, e.Subject, e.Object, e.Decision, e.Rev)
		}
	}
	p.history = append([]PoolApply{a}, p.history...)
	if len(p.history) > maxPoolApplies {
		p.history = p.history[:maxPoolApplies]
	}
	return a
}

type PoolCheckResult struct {
	Subject    string `json:"subject"`
	Object     string `json:"object"`
	Decision   string `json:"decision"`
	Expected   string `json:"expected"`
	Correct    bool   `json:"correct"`
	ServedBy   string `json:"servedBy"` // cache | primary
	CacheRev   int64  `json:"cacheRev"`
	PrimaryRev int64  `json:"primaryRev"`
	// Pending counts the queued or stalled events for this user/document.
	Pending int `json:"pending"`
	// Stuck: the answer is stale and no pending event will change it.
	Stuck  bool   `json:"stuck"`
	Reason string `json:"reason"`
}

// pending counts queued and stalled events for subject/object.
func (p *Pools) pending(subject, object string) int {
	n := 0
	for i, q := range p.queues {
		for _, e := range q {
			if e.Subject == subject && e.Object == object {
				n++
			}
		}
		if e := p.inflight[i]; e != nil && e.Subject == subject && e.Object == object {
			n++
		}
	}
	return n
}

// Check serves a cache hit; a miss reads SQLite and caches the decision.
func (p *Pools) Check(subject, object string) (PoolCheckResult, error) {
	if err := validate(subject, object); err != nil {
		return PoolCheckResult{}, err
	}
	truth, err := p.Store.CanView(subject, object)
	if err != nil {
		return PoolCheckResult{}, err
	}
	r := PoolCheckResult{Subject: subject, Object: object, Expected: decision(truth), PrimaryRev: p.Store.Rev(), Pending: p.pending(subject, object)}
	key := cacheKey(subject, object)
	if e, ok := p.cache[key]; ok {
		r.ServedBy, r.Decision, r.CacheRev = "cache", e.Decision, e.Rev
	} else {
		r.ServedBy, r.Decision, r.CacheRev = "primary", r.Expected, r.PrimaryRev
		p.cache[key] = CacheEntry{Subject: subject, Object: object, Decision: r.Decision, Rev: r.PrimaryRev}
	}
	r.Correct = r.Decision == r.Expected
	switch {
	case r.ServedBy == "primary":
		r.Reason = fmt.Sprintf("Cache miss: SQLite answered %s at rev %d and the decision was cached.", r.Decision, r.PrimaryRev)
	case r.Correct:
		r.Reason = fmt.Sprintf("Cache hit: %s @ rev %d agrees with SQLite.", r.Decision, r.CacheRev)
	case r.Pending > 0:
		r.Reason = fmt.Sprintf("Stale: the cache serves %s @ rev %d, SQLite says %s at rev %d. %d event%s for this entry %s still pending.", r.Decision, r.CacheRev, r.Expected, r.PrimaryRev, r.Pending, plural(r.Pending), map[bool]string{true: "is", false: "are"}[r.Pending == 1])
	default:
		r.Stuck = true
		r.Reason = fmt.Sprintf("Stale, and stuck: the cache serves %s @ rev %d, SQLite says %s at rev %d, and every event has been applied. Nothing will correct this entry until its TTL (%d s) expires.", r.Decision, r.CacheRev, r.Expected, r.PrimaryRev, CacheTTLSec)
	}
	return r, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

type PoolsState struct {
	Mode       string                `json:"mode"`
	Routing    string                `json:"routing"`
	PrimaryRev int64                 `json:"primaryRev"`
	Tuples     []string              `json:"tuples"`
	Queues     [NumPools][]PoolEvent `json:"queues"`
	Inflight   [NumPools]*PoolEvent  `json:"inflight"`
	Cache      []CacheEntry          `json:"cache"`
	History    []PoolApply           `json:"history"`
	// DocPool is the pool doc:42's events go to with routing "document".
	DocPool int `json:"docPool"`
	TTLSec  int `json:"ttlSec"`
}

func (p *Pools) State() (PoolsState, error) {
	s := PoolsState{Mode: p.Mode, Routing: p.Routing, PrimaryRev: p.Store.Rev(), Tuples: []string{}, Cache: []CacheEntry{},
		History: append([]PoolApply{}, p.history...), Inflight: p.inflight, DocPool: DocPool("doc:42"), TTLSec: CacheTTLSec}
	for i, q := range p.queues {
		s.Queues[i] = append([]PoolEvent{}, q...)
	}
	tuples, err := p.Store.Tuples()
	if err != nil {
		return s, err
	}
	for _, t := range tuples {
		s.Tuples = append(s.Tuples, t.String())
	}
	for _, e := range p.cache {
		s.Cache = append(s.Cache, e)
	}
	sort.Slice(s.Cache, func(i, j int) bool {
		return cacheKey(s.Cache[i].Subject, s.Cache[i].Object) < cacheKey(s.Cache[j].Subject, s.Cache[j].Object)
	})
	return s, nil
}
