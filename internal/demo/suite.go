package demo

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
)

// TestCase is one correctness check, runnable from the UI and from `go test`.
type TestCase struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // scenario | property
	Strategy    Strategy `json:"strategy,omitempty"`
	Description string   `json:"description"`
	Passed      bool     `json:"passed"`
	Details     []string `json:"details"`
}

type StrategyStats struct {
	Checks      int `json:"checks"`
	Wrong       int `json:"wrong"`       // decisions that differ from SQLite
	StaleAllows int `json:"staleAllows"` // ALLOW where SQLite says DENY
	InWindow    int `json:"inWindow"`    // wrong decisions made while the user's invalidation was still queued
	CacheHits   int `json:"cacheHits"`
	Fallbacks   int `json:"fallbacks"` // stale entries refused (revision-aware only)
	// P50Ms and P99Ms are latency percentiles over every check (nearest rank).
	P50Ms      float64 `json:"p50Ms"`
	P99Ms      float64 `json:"p99Ms"`
	HitRatePct float64 `json:"hitRatePct"`

	latencies []float64
}

// observe records one check's latency.
func (s *StrategyStats) observe(ms float64) { s.latencies = append(s.latencies, ms) }

// percentile is the nearest-rank p-th percentile of the recorded latencies.
func (s *StrategyStats) percentile(p float64) float64 {
	if len(s.latencies) == 0 {
		return 0
	}
	v := slices.Clone(s.latencies)
	slices.Sort(v)
	rank := int(math.Ceil(p / 100 * float64(len(v))))
	return v[max(rank, 1)-1]
}

type PropertyStats struct {
	Seeds      int                         `json:"seeds"`
	OpsPerRun  int                         `json:"opsPerRun"`
	Writes     int                         `json:"writes"`
	Deliveries int                         `json:"deliveries"`
	ByStrat    map[Strategy]*StrategyStats `json:"byStrategy"`
	// FullyConsistent is the same workload checked with fully consistent
	// reads (revision-aware strategy, every check on SQLite).
	FullyConsistent *StrategyStats `json:"fullyConsistent"`
}

type SuiteResult struct {
	Passed     bool          `json:"passed"`
	Cases      []TestCase    `json:"cases"`
	Property   PropertyStats `json:"property"`
	DurationMs float64       `json:"durationMs"`
}

// Story runs the five-step story for one strategy: Alice reads, the admin
// revokes her, Alice reads again, a fix-up step runs, and Alice reads a last
// time. The fix-up step delivers the next invalidation, except for the TTL
// strategy with EnforceTTL, where it fast-forwards the clock past the TTL.
// With AutoDeliver the invalidation is delivered right after the commit.
func Story(s Strategy, o Options) (before, after CheckResult, err error) {
	d, err := New(s)
	if err != nil {
		return
	}
	defer d.Close()
	d.Options = o
	if _, err = d.Check("user:alice", "doc:42"); err != nil {
		return
	}
	if _, err = d.Revoke("user:alice", "doc:42"); err != nil {
		return
	}
	if before, err = d.Check("user:alice", "doc:42"); err != nil {
		return
	}
	if s == TTL && o.EnforceTTL {
		d.Advance(CacheTTLSec)
	} else {
		d.DeliverNext()
	}
	after, err = d.Check("user:alice", "doc:42")
	return
}

func describe(r CheckResult) string {
	s := fmt.Sprintf("%s: %s view %s → %s (SQLite says %s), served by %s, %.2f ms", r.Strategy, r.Subject, r.Object, r.Decision, r.Expected, r.ServedBy, r.LatencyMs)
	if r.Pending != nil {
		s += fmt.Sprintf(", invalidation #%d queued at position %d of %d", r.Pending.ID, r.PendingPos, r.QueueDepth)
	}
	return s
}

func scenarioCases() ([]TestCase, error) {
	var cases []TestCase
	add := func(s Strategy, name, desc string, passed bool, details ...string) {
		cases = append(cases, TestCase{Name: name, Kind: "scenario", Strategy: s, Description: desc, Passed: passed, Details: details})
	}

	ttlBefore, ttlAfter, err := Story(TTL, Options{})
	if err != nil {
		return nil, err
	}
	add(TTL, "TTL cache serves a stale ALLOW until the TTL expires",
		"After Alice is revoked, the TTL cache answers ALLOW both before and after the delivery step (it sends no invalidations). This test passes when the problem is reproduced.",
		!ttlBefore.Correct && !ttlAfter.Correct && ttlAfter.ServedBy == "cache",
		describe(ttlBefore), describe(ttlAfter))

	expBefore, expAfter, err := Story(TTL, Options{EnforceTTL: true})
	if err != nil {
		return nil, err
	}
	add(TTL, "TTL cache: the stale window ends when the entry's TTL runs out",
		fmt.Sprintf("With TTL expiry on, the read right after the revoke is a stale cache hit; after fast-forwarding the clock %d s the entry is evicted and SQLite answers DENY.", CacheTTLSec),
		!expBefore.Correct && expBefore.ServedBy == "cache" && expBefore.EntryAgeSec < CacheTTLSec &&
			expAfter.Correct && expAfter.Decision == "DENY" && expAfter.ServedBy == "primary",
		describe(expBefore), describe(expAfter))

	edge, err := ttlEdge()
	if err != nil {
		return nil, err
	}
	add(TTL, "TTL cache: an entry is served until exactly its TTL",
		fmt.Sprintf("On a frozen clock, the stale entry is still served %d s after it was cached and evicted at %d s.", CacheTTLSec-1, CacheTTLSec),
		!edge[0].Correct && edge[0].ServedBy == "cache" && edge[1].Correct && edge[1].ServedBy == "primary",
		fmt.Sprintf("at %d s: %s", CacheTTLSec-1, describe(edge[0])), fmt.Sprintf("at %d s: %s", CacheTTLSec, describe(edge[1])))

	invBefore, invAfter, err := Story(Invalidation, Options{})
	if err != nil {
		return nil, err
	}
	add(Invalidation, "Invalidation: a read right after the write is stale while the event is queued",
		"SQLite has committed the revoke, but its invalidation is still in the delivery queue, so Alice's read hits the old cached ALLOW: read-after-write inconsistency.",
		!invBefore.Correct && invBefore.Pending != nil && invBefore.ServedBy == "cache",
		describe(invBefore))
	autoBefore, _, err := Story(Invalidation, Options{AutoDeliver: true})
	if err != nil {
		return nil, err
	}
	add(Invalidation, "Invalidation with automatic delivery (flag off) reads fresh after the revoke",
		"With controlled delivery disabled, the event is delivered right after the commit, so the next read already goes to SQLite.",
		autoBefore.Correct && autoBefore.Decision == "DENY" && autoBefore.Pending == nil,
		describe(autoBefore))
	add(Invalidation, "Invalidation: delivering the event fixes the cache",
		"Once the invalidation is delivered, the entry is gone, so the next read goes to SQLite and returns DENY: mitigated, not guaranteed.",
		invAfter.Correct && invAfter.Decision == "DENY" && invAfter.Pending == nil,
		describe(invAfter))

	hpBefore, hpAfter, err := Story(Invalidation, Options{HappyPath: true})
	if err != nil {
		return nil, err
	}
	hpDetails := []string{describe(hpBefore)}
	if hp := hpBefore.HappyPath; hp != nil {
		hpDetails = append(hpDetails, fmt.Sprintf("happy path: %s, served by %s, %.2f ms", hp.Decision, hp.ServedBy, hp.LatencyMs))
	}
	add(Invalidation, "Invalidation: a read before delivery reports the happy-path result next to its own",
		"While Alice's invalidation is queued, the response carries both the stale cached ALLOW and the happy-path DENY (event delivered first). After delivery there is nothing to compare.",
		!hpBefore.Correct && hpBefore.HappyPath != nil && hpBefore.HappyPath.Decision == "DENY" && hpBefore.HappyPath.Correct &&
			hpAfter.Correct && hpAfter.HappyPath == nil,
		hpDetails...)

	d, err := New(Invalidation)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	d.Check("user:alice", "doc:42")
	if _, err := d.AddOtherWrites(3); err != nil {
		return nil, err
	}
	d.Revoke("user:alice", "doc:42")
	var reads []CheckResult
	for range 4 {
		d.DeliverNext()
		r, err := d.Check("user:alice", "doc:42")
		if err != nil {
			return nil, err
		}
		reads = append(reads, r)
	}
	staleUntilHer := !reads[0].Correct && !reads[1].Correct && !reads[2].Correct && reads[3].Correct
	details := []string{}
	for i, r := range reads {
		details = append(details, fmt.Sprintf("after %d deliveries: %s", i+1, describe(r)))
	}
	add(Invalidation, "Invalidation: a queue backlog stretches the inconsistency",
		"Alice's invalidation is queued behind 3 unrelated ones. Her reads stay stale through 3 deliveries and become fresh only when her own event is delivered.",
		staleUntilHer, details...)

	raBefore, raAfter, err := Story(RevisionAware, Options{})
	if err != nil {
		return nil, err
	}
	add(RevisionAware, "Revision-aware is correct before any delivery",
		"The cached ALLOW (rev 3) is older than doc:42's token (ver#4), so the check falls back to SQLite and answers DENY. No invalidation is needed.",
		raBefore.Correct && raBefore.Decision == "DENY" && raBefore.ServedBy == "primary",
		describe(raBefore))
	add(RevisionAware, "Revision-aware pays the fallback once, then hits the cache again",
		"The fallback refreshed the cache at rev 4, so the next check is a fast, correct cache hit.",
		raAfter.Correct && raAfter.ServedBy == "cache",
		describe(raAfter))
	tok, err := tokenStory()
	if err != nil {
		return nil, err
	}
	add(RevisionAware, "Revision-aware: an old token in the request can still read a result that is not the latest",
		fmt.Sprintf("Alice's client read doc:42 at ver#%d and still sends that token after the revoke moved the doc to ver#%d. The cached ALLOW (rev %d) satisfies the token, so it is served: the promise (evaluated at a version ≥ the token) is kept, but the result is not the latest.", tok.old.RequiredRev, tok.old.LatestToken, tok.old.CacheRev),
		tok.old.TokenSource == "client" && tok.old.ServedBy == "cache" && tok.old.Decision == "ALLOW" && tok.old.Expected == "DENY" && tok.old.ContractMet,
		describe(tok.old), fmt.Sprintf("evaluated at rev %d ≥ request token ver#%d; latest ver#%d", tok.old.EvaluatedRev, tok.old.RequiredRev, tok.old.LatestToken))
	add(RevisionAware, "Revision-aware: the latest token makes the same check fresh",
		"With doc:42's latest token, the cached entry is too old, so the check falls back to SQLite and answers DENY.",
		tok.latest.Correct && tok.latest.Decision == "DENY" && tok.latest.ServedBy == "primary" && tok.latest.ContractMet,
		describe(tok.latest))
	strongNoCache := true
	for _, h := range tok.strong.Hops {
		strongNoCache = strongNoCache && h.To != "cache" && h.From != "cache"
	}
	add(RevisionAware, "Fully consistent: always the latest, never from the cache",
		fmt.Sprintf("A fully consistent check skips the cache and asks SQLite at its latest revision: correct, at %.1f ms instead of %.1f ms for a cache hit.", tok.strong.LatencyMs, 2*NetworkMs+CacheHitMs),
		tok.strong.Correct && tok.strong.ServedBy == "primary" && tok.strong.Consistency == "fully_consistent" && strongNoCache,
		describe(tok.strong))

	out, err := outageStory()
	if err != nil {
		return nil, err
	}
	add(RevisionAware, "SQLite outage: a snapshot at least as fresh as the request's token still answers",
		fmt.Sprintf("Alice's ALLOW was cached at rev %d. With SQLite down, her check with token ver#%d is answered from that snapshot: no SQLite hop, still correct.", out.old.CacheRev, out.old.RequiredRev),
		out.old.PrimaryDown && out.old.ServedBy == "cache" && out.old.Correct && out.old.ContractMet && !touches(out.old.Hops, "primary"),
		describe(out.old))
	unavailable := []string{}
	for _, e := range out.failed {
		unavailable = append(unavailable, fmt.Sprintf("%s → 503: %s", e.Subject, e.Message))
	}
	add(RevisionAware, "SQLite outage: no snapshot satisfies the revision → 503",
		"With SQLite down, the API returns 503 instead of guessing when the cached snapshot is older than the requested version, when nothing is cached, for a fully consistent check, and for a write.",
		len(out.failed) == 4 &&
			out.failed[0].SnapshotRev == out.old.CacheRev && out.failed[0].RequiredRev > out.failed[0].SnapshotRev &&
			out.failed[1].SnapshotRev == 0 && out.failed[2].Consistency == "fully_consistent" &&
			strings.Contains(out.failed[0].Message, "no snapshot satisfies the revision"),
		unavailable...)
	add(RevisionAware, "SQLite recovery: the next check reads SQLite again",
		"Once SQLite is back, the check with the latest token that got a 503 falls back to SQLite, answers correctly and refreshes the snapshot.",
		out.recovered.ServedBy == "primary" && out.recovered.Correct && !out.recovered.PrimaryDown,
		describe(out.recovered))
	return cases, nil
}

func touches(hops []Hop, box string) bool {
	for _, h := range hops {
		if h.From == box || h.To == box {
			return true
		}
	}
	return false
}

type outageResults struct {
	old       CheckResult
	failed    []*UnavailableError
	recovered CheckResult
}

// outageStory: Alice reads doc:42 (her client keeps ver#3), Carol is granted
// doc:42 (ver#4), then SQLite goes down. Alice checks with her token, then
// requests that no snapshot satisfies are made, then SQLite comes back.
func outageStory() (outageResults, error) {
	var out outageResults
	d, err := New(RevisionAware)
	if err != nil {
		return out, err
	}
	defer d.Close()
	d.ClientTokens = true
	first, err := d.Check("user:alice", "doc:42")
	if err != nil {
		return out, err
	}
	if _, err := d.Grant("user:carol", "doc:42"); err != nil {
		return out, err
	}
	d.SetPrimaryDown(true)
	if out.old, err = d.CheckWith("user:alice", "doc:42", CheckOpts{Token: first.LatestToken}); err != nil {
		return out, err
	}
	for _, f := range []func() error{
		func() error { _, err := d.Check("user:alice", "doc:42"); return err },
		func() error { _, err := d.Check("user:carol", "doc:42"); return err },
		func() error {
			_, err := d.CheckWith("user:alice", "doc:42", CheckOpts{FullyConsistent: true})
			return err
		},
		func() error { _, err := d.Revoke("user:carol", "doc:42"); return err },
	} {
		var ue *UnavailableError
		if err := f(); !errors.As(err, &ue) {
			return out, fmt.Errorf("want a 503 while SQLite is down, got %v", err)
		}
		out.failed = append(out.failed, ue)
	}
	d.SetPrimaryDown(false)
	out.recovered, err = d.Check("user:alice", "doc:42")
	return out, err
}

type tokenResults struct{ old, latest, strong CheckResult }

// tokenStory: Alice reads doc:42 (her client keeps the doc's token), the
// admin revokes her, then her client checks with that old token, with the
// latest token, and fully consistently.
func tokenStory() (tokenResults, error) {
	var out tokenResults
	d, err := New(RevisionAware)
	if err != nil {
		return out, err
	}
	defer d.Close()
	d.ClientTokens = true
	first, err := d.Check("user:alice", "doc:42")
	if err != nil {
		return out, err
	}
	if _, err := d.Revoke("user:alice", "doc:42"); err != nil {
		return out, err
	}
	if out.old, err = d.CheckWith("user:alice", "doc:42", CheckOpts{Token: first.LatestToken}); err != nil {
		return out, err
	}
	if out.strong, err = d.CheckWith("user:alice", "doc:42", CheckOpts{FullyConsistent: true}); err != nil {
		return out, err
	}
	out.latest, err = d.Check("user:alice", "doc:42")
	return out, err
}

// ttlEdge reads a stale entry one second before and exactly at its TTL, on a frozen clock.
func ttlEdge() ([2]CheckResult, error) {
	var out [2]CheckResult
	d, err := New(TTL)
	if err != nil {
		return out, err
	}
	defer d.Close()
	now := time.Unix(0, 0)
	d.Now = func() time.Time { return now }
	d.EnforceTTL = true
	if _, err := d.Check("user:alice", "doc:42"); err != nil {
		return out, err
	}
	if _, err := d.Revoke("user:alice", "doc:42"); err != nil {
		return out, err
	}
	now = now.Add((CacheTTLSec - 1) * time.Second)
	if out[0], err = d.Check("user:alice", "doc:42"); err != nil {
		return out, err
	}
	now = now.Add(time.Second)
	out[1], err = d.Check("user:alice", "doc:42")
	return out, err
}

// propertyRun replays one random workload against all strategies. Writes,
// deliveries and checks are generated once from the seed, so every strategy
// sees exactly the same sequence of operations.
func propertyRun(seed uint64, ops int, ps *PropertyStats) error {
	rng := rand.New(rand.NewPCG(seed, 0xa11ce))
	demos := make([]*Demo, len(Strategies))
	for i, s := range Strategies {
		d, err := New(s)
		if err != nil {
			return err
		}
		defer d.Close()
		demos[i] = d
	}
	// The same workload, checked fully consistently.
	strong, err := New(RevisionAware)
	if err != nil {
		return err
	}
	defer strong.Close()
	strong.ClientTokens = true
	all := append(demos[:len(demos):len(demos)], strong)
	viewers := map[string]bool{"user:alice|doc:42": true}

	for range ops {
		u := Users[rng.IntN(len(Users))]
		doc := Docs[rng.IntN(len(Docs))]
		key := u + "|" + doc
		r := rng.Float64()
		switch {
		case r < 0.05 && !viewers[key]:
			viewers[key] = true
			for _, d := range all {
				if _, err := d.Grant(u, doc); err != nil {
					return err
				}
			}
			ps.Writes++
		case r < 0.10 && viewers[key]:
			delete(viewers, key)
			for _, d := range all {
				if _, err := d.Revoke(u, doc); err != nil {
					return err
				}
			}
			ps.Writes++
		case r < 0.25:
			// Deliver the next queued invalidation (a no-op for strategies without a queue).
			for _, d := range demos {
				if d.DeliverNext().Delivered {
					ps.Deliveries++
				}
			}
		default:
			res, err := strong.CheckWith(u, doc, CheckOpts{FullyConsistent: true})
			if err != nil {
				return err
			}
			fc := ps.FullyConsistent
			fc.Checks++
			fc.observe(res.LatencyMs)
			if !res.Correct {
				fc.Wrong++
			}
			if res.ServedBy == "cache" {
				fc.CacheHits++
			}
			for _, d := range demos {
				res, err := d.Check(u, doc)
				if err != nil {
					return err
				}
				s := ps.ByStrat[d.Strategy]
				s.Checks++
				s.observe(res.LatencyMs)
				if !res.Correct {
					s.Wrong++
					if res.Pending != nil {
						s.InWindow++
					}
				}
				if res.Decision == "ALLOW" && res.Expected == "DENY" {
					s.StaleAllows++
				}
				if res.ServedBy == "cache" {
					s.CacheHits++
				}
				if res.ServedBy == "primary" && res.CacheRev > 0 {
					s.Fallbacks++
				}
			}
		}
	}
	return nil
}

func propertyCases() ([]TestCase, PropertyStats, error) {
	ps := PropertyStats{Seeds: 30, OpsPerRun: 400, ByStrat: map[Strategy]*StrategyStats{}, FullyConsistent: &StrategyStats{}}
	for _, s := range Strategies {
		ps.ByStrat[s] = &StrategyStats{}
	}
	for seed := uint64(1); seed <= uint64(ps.Seeds); seed++ {
		if err := propertyRun(seed, ps.OpsPerRun, &ps); err != nil {
			return nil, ps, err
		}
	}
	for _, s := range append([]*StrategyStats{ps.FullyConsistent}, values(ps.ByStrat)...) {
		if s.Checks > 0 {
			s.P50Ms, s.P99Ms = s.percentile(50), s.percentile(99)
			s.HitRatePct = 100 * float64(s.CacheHits) / float64(s.Checks)
		}
	}
	summary := func(s Strategy) string {
		st := ps.ByStrat[s]
		return fmt.Sprintf("%s: %d checks, %d wrong (%d stale ALLOWs, %d while the invalidation was queued), cache hit rate %.1f%%, %d fallbacks, p50 %.2f ms, p99 %.2f ms",
			s, st.Checks, st.Wrong, st.StaleAllows, st.InWindow, st.HitRatePct, st.Fallbacks, st.P50Ms, st.P99Ms)
	}
	ttl, inv, ra, fc := ps.ByStrat[TTL], ps.ByStrat[Invalidation], ps.ByStrat[RevisionAware], ps.FullyConsistent
	workload := fmt.Sprintf("%d random workloads × %d operations (grants, revokes, invalidation deliveries and checks for %d users on %d documents)", ps.Seeds, ps.OpsPerRun, len(Users), len(Docs))
	cases := []TestCase{
		{
			Name: "Property: revision-aware decisions always match SQLite", Kind: "property", Strategy: RevisionAware,
			Description: workload + ". Every check carries the document's latest version token, and every revision-aware decision must equal SQLite's at the moment of the check.",
			Passed:      ra.Checks > 0 && ra.Wrong == 0,
			Details:     []string{summary(RevisionAware)},
		},
		{
			Name: "Property: invalidation mitigates, but cannot eliminate, stale decisions", Kind: "property", Strategy: Invalidation,
			Description: "On the same workloads, invalidation must make far fewer mistakes than TTL, yet still some, and every one of them must happen while that user's invalidation was still queued.",
			Passed:      inv.Wrong > 0 && inv.Wrong < ttl.Wrong && inv.InWindow == inv.Wrong,
			Details:     []string{summary(Invalidation)},
		},
		{
			Name: "Property: fully consistent reads are always correct, but never use the cache", Kind: "property", Strategy: RevisionAware,
			Description: "The same workloads, checked fully consistently: every decision matches SQLite, but the cache hit rate is 0%. At p99 both pay one SQLite round trip; the difference is that a fully consistent read pays it on every check, so its median is the SQLite path too and SQLite serves all the read load. Latest-ness costs scalability.",
			Passed:      fc.Checks > 0 && fc.Wrong == 0 && fc.CacheHits == 0 && fc.P50Ms > ra.P50Ms,
			Details: []string{fmt.Sprintf("fully consistent: %d checks, %d wrong, cache hit rate %.1f%% (all %d checks read SQLite), p50 %.2f ms, p99 %.2f ms", fc.Checks, fc.Wrong, fc.HitRatePct, fc.Checks, fc.P50Ms, fc.P99Ms),
				fmt.Sprintf("revision-aware: %d of %d checks read SQLite, p50 %.2f ms, p99 %.2f ms", ra.Checks-ra.CacheHits, ra.Checks, ra.P50Ms, ra.P99Ms)},
		},
		{
			Name: "Property sanity: the same workloads catch the TTL cache", Kind: "property", Strategy: TTL,
			Description: "A test that can't fail proves nothing. The identical workloads must detect stale decisions in the TTL cache.",
			Passed:      ttl.StaleAllows > 0,
			Details:     []string{summary(TTL)},
		},
	}
	return cases, ps, nil
}

func values(m map[Strategy]*StrategyStats) []*StrategyStats {
	var out []*StrategyStats
	for _, s := range Strategies {
		out = append(out, m[s])
	}
	return out
}

// RunSuite runs every correctness test.
func RunSuite() (SuiteResult, error) {
	start := time.Now()
	sc, err := scenarioCases()
	if err != nil {
		return SuiteResult{}, err
	}
	pc, ps, err := propertyCases()
	if err != nil {
		return SuiteResult{}, err
	}
	res := SuiteResult{Cases: append(sc, pc...), Property: ps, Passed: true}
	for _, c := range res.Cases {
		res.Passed = res.Passed && c.Passed
	}
	res.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	return res, nil
}
