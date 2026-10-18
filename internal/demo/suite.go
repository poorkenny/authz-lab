package demo

import (
	"fmt"
	"math/rand/v2"
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
	Checks      int     `json:"checks"`
	Wrong       int     `json:"wrong"`       // decisions that differ from SQLite
	StaleAllows int     `json:"staleAllows"` // ALLOW where SQLite says DENY
	InWindow    int     `json:"inWindow"`    // wrong decisions made while an invalidation was in flight
	CacheHits   int     `json:"cacheHits"`
	Fallbacks   int     `json:"fallbacks"` // stale entries refused (revision-aware only)
	MeanMs      float64 `json:"meanMs"`
	HitRatePct  float64 `json:"hitRatePct"`
}

type PropertyStats struct {
	Seeds     int                         `json:"seeds"`
	OpsPerRun int                         `json:"opsPerRun"`
	Writes    int                         `json:"writes"`
	ByStrat   map[Strategy]*StrategyStats `json:"byStrategy"`
}

type SuiteResult struct {
	Passed     bool          `json:"passed"`
	Cases      []TestCase    `json:"cases"`
	Property   PropertyStats `json:"property"`
	DurationMs float64       `json:"durationMs"`
}

// Story runs the four-step story for one strategy: Alice reads, the admin
// revokes her, Alice retries 10 ms later (inside the invalidation window),
// and again 1 s later.
func Story(s Strategy) (soon, later CheckResult, err error) {
	d, err := New(s)
	if err != nil {
		return
	}
	defer d.Close()
	if _, err = d.Check("user:alice", "doc:42"); err != nil {
		return
	}
	d.Advance(1000)
	if _, err = d.Revoke("user:alice", "doc:42"); err != nil {
		return
	}
	d.Advance(10)
	if soon, err = d.Check("user:alice", "doc:42"); err != nil {
		return
	}
	d.Advance(1000)
	later, err = d.Check("user:alice", "doc:42")
	return
}

func describe(r CheckResult) string {
	s := fmt.Sprintf("t=%d ms %s: %s view %s → %s (SQLite says %s), served by %s, %.2f ms", r.T, r.Strategy, r.Subject, r.Object, r.Decision, r.Expected, r.ServedBy, r.LatencyMs)
	if r.RaceWindow {
		s += fmt.Sprintf(", invalidation in flight until t=%d ms", r.Invalidation.DeliverAt)
	}
	return s
}

func scenarioCases() ([]TestCase, error) {
	var cases []TestCase
	add := func(s Strategy, name, desc string, passed bool, details ...string) {
		cases = append(cases, TestCase{Name: name, Kind: "scenario", Strategy: s, Description: desc, Passed: passed, Details: details})
	}

	ttlSoon, ttlLater, err := Story(TTL)
	if err != nil {
		return nil, err
	}
	add(TTL, "TTL cache serves a stale ALLOW for the whole TTL",
		"After Alice is revoked, the TTL cache answers ALLOW both 10 ms and 1 s later. This test passes when the problem is reproduced.",
		!ttlSoon.Correct && !ttlLater.Correct && ttlLater.ServedBy == "cache",
		describe(ttlSoon), describe(ttlLater))

	invSoon, invLater, err := Story(Invalidation)
	if err != nil {
		return nil, err
	}
	add(Invalidation, "Invalidation still serves a stale ALLOW inside the race window",
		"A check 10 ms after the SQLite commit lands before the invalidation (80 ms) arrives, so it still hits the old entry. This can't be prevented, only shortened.",
		!invSoon.Correct && invSoon.RaceWindow && invSoon.ServedBy == "cache",
		describe(invSoon))
	add(Invalidation, "Invalidation fixes the cache once it arrives",
		"1 s later the invalidation has removed the entry, so the check reads SQLite and answers DENY: mitigated, not guaranteed.",
		invLater.Correct && invLater.Decision == "DENY" && !invLater.RaceWindow,
		describe(invLater))

	raSoon, raLater, err := Story(RevisionAware)
	if err != nil {
		return nil, err
	}
	add(RevisionAware, "Revision-aware is correct even inside the race window",
		"10 ms after the commit the cached ALLOW (rev 3) is older than doc:42's token (rev 4), so the check falls back to SQLite and answers DENY.",
		raSoon.Correct && raSoon.Decision == "DENY" && raSoon.ServedBy == "primary",
		describe(raSoon))
	add(RevisionAware, "Revision-aware pays the fallback once, then hits the cache again",
		"The fallback refreshed the cache at rev 4, so the next check is a fast, correct cache hit.",
		raLater.Correct && raLater.ServedBy == "cache",
		describe(raLater))

	d, err := New(RevisionAware)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	d.Check("user:alice", "doc:42")
	d.Revoke("user:alice", "doc:42")
	d.Check("user:alice", "doc:42")
	d.Grant("user:alice", "doc:42")
	regrant, err := d.Check("user:alice", "doc:42")
	if err != nil {
		return nil, err
	}
	add(RevisionAware, "Revision-aware: a re-grant is visible immediately",
		"Stale DENYs are refused too: the cached DENY predates the re-grant's token, so Alice gets ALLOW.",
		regrant.Correct && regrant.Decision == "ALLOW",
		describe(regrant))
	return cases, nil
}

// propertyRun replays one random workload against all strategies. Writes,
// checks and clock advances are generated once, so every strategy sees
// exactly the same operations at the same simulated times.
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
	viewers := map[string]bool{"user:alice|doc:42": true}

	for range ops {
		// Requests arrive 0-40 ms apart, so some land inside the 80 ms
		// invalidation window and some after it.
		gap := rng.Int64N(41)
		for _, d := range demos {
			d.Advance(gap)
		}
		u := Users[rng.IntN(len(Users))]
		doc := Docs[rng.IntN(len(Docs))]
		key := u + "|" + doc
		r := rng.Float64()
		switch {
		case r < 0.05 && !viewers[key]:
			viewers[key] = true
			for _, d := range demos {
				if _, err := d.Grant(u, doc); err != nil {
					return err
				}
			}
			ps.Writes++
		case r < 0.10 && viewers[key]:
			delete(viewers, key)
			for _, d := range demos {
				if _, err := d.Revoke(u, doc); err != nil {
					return err
				}
			}
			ps.Writes++
		default:
			for _, d := range demos {
				res, err := d.Check(u, doc)
				if err != nil {
					return err
				}
				s := ps.ByStrat[d.Strategy]
				s.Checks++
				s.MeanMs += res.LatencyMs
				if !res.Correct {
					s.Wrong++
					if res.RaceWindow {
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
	ps := PropertyStats{Seeds: 30, OpsPerRun: 400, ByStrat: map[Strategy]*StrategyStats{}}
	for _, s := range Strategies {
		ps.ByStrat[s] = &StrategyStats{}
	}
	for seed := uint64(1); seed <= uint64(ps.Seeds); seed++ {
		if err := propertyRun(seed, ps.OpsPerRun, &ps); err != nil {
			return nil, ps, err
		}
	}
	for _, s := range ps.ByStrat {
		if s.Checks > 0 {
			s.MeanMs /= float64(s.Checks)
			s.HitRatePct = 100 * float64(s.CacheHits) / float64(s.Checks)
		}
	}
	summary := func(s Strategy) string {
		st := ps.ByStrat[s]
		return fmt.Sprintf("%s: %d checks, %d wrong (%d stale ALLOWs, %d inside an invalidation window), cache hit rate %.1f%%, %d fallbacks, mean latency %.2f ms",
			s, st.Checks, st.Wrong, st.StaleAllows, st.InWindow, st.HitRatePct, st.Fallbacks, st.MeanMs)
	}
	ttl, inv, ra := ps.ByStrat[TTL], ps.ByStrat[Invalidation], ps.ByStrat[RevisionAware]
	workload := fmt.Sprintf("%d random workloads × %d operations (grants, revokes and checks for %d users on %d documents, 0-40 ms apart)", ps.Seeds, ps.OpsPerRun, len(Users), len(Docs))
	cases := []TestCase{
		{
			Name: "Property: revision-aware decisions always match SQLite", Kind: "property", Strategy: RevisionAware,
			Description: workload + ". Every revision-aware decision must equal SQLite's at the moment of the check.",
			Passed:      ra.Checks > 0 && ra.Wrong == 0,
			Details:     []string{summary(RevisionAware)},
		},
		{
			Name: "Property: invalidation mitigates, but cannot eliminate, stale decisions", Kind: "property", Strategy: Invalidation,
			Description: "On the same workloads, invalidation must make far fewer mistakes than TTL, yet still some, and every one of them must fall inside an invalidation's propagation window.",
			Passed:      inv.Wrong > 0 && inv.Wrong < ttl.Wrong && inv.InWindow == inv.Wrong,
			Details:     []string{summary(Invalidation)},
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
