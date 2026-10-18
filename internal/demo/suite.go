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
	Description string   `json:"description"`
	Passed      bool     `json:"passed"`
	Details     []string `json:"details"`
}

type ModeStats struct {
	Checks      int     `json:"checks"`
	Wrong       int     `json:"wrong"`       // decisions that differ from the primary
	StaleAllows int     `json:"staleAllows"` // ALLOW where the primary says DENY
	CacheHits   int     `json:"cacheHits"`
	Fallbacks   int     `json:"fallbacks"` // stale entries refused (revision-aware only)
	MeanMs      float64 `json:"meanMs"`
	HitRatePct  float64 `json:"hitRatePct"`
}

type PropertyStats struct {
	Seeds     int       `json:"seeds"`
	OpsPerRun int       `json:"opsPerRun"`
	Writes    int       `json:"writes"`
	Plain     ModeStats `json:"plain"`
	Aware     ModeStats `json:"revisionAware"`
}

type SuiteResult struct {
	Passed     bool          `json:"passed"`
	Cases      []TestCase    `json:"cases"`
	Property   PropertyStats `json:"property"`
	DurationMs float64       `json:"durationMs"`
}

// The three-step story: Alice reads (cached ALLOW), the admin revokes her,
// Alice reads again.
func story(mode Mode) (first, afterRevoke CheckResult, d *Demo) {
	d = New(mode)
	first, _ = d.Check("user:alice", "doc:42")
	if _, err := d.Revoke("user:alice", "doc:42"); err != nil {
		panic(err)
	}
	afterRevoke, _ = d.Check("user:alice", "doc:42")
	return first, afterRevoke, d
}

func describe(r CheckResult) string {
	return fmt.Sprintf("%s %s %s → %s (expected %s), served by %s, cache rev %d, token %d, %.2f ms",
		r.Mode, r.Subject, r.Object, r.Decision, r.Expected, r.ServedBy, r.CacheRev, r.RequiredRev, r.LatencyMs)
}

func scenarioCases() []TestCase {
	var cases []TestCase
	add := func(name, desc string, passed bool, details ...string) {
		cases = append(cases, TestCase{Name: name, Kind: "scenario", Description: desc, Passed: passed, Details: details})
	}

	_, plainAfter, _ := story(PlainCache)
	add("Plain cache reproduces the stale ALLOW",
		"After Alice is revoked, the plain cache still answers ALLOW from its pre-revoke entry. This test passes when the bug is reproduced.",
		plainAfter.Decision == "ALLOW" && plainAfter.Expected == "DENY" && plainAfter.ServedBy == "cache",
		describe(plainAfter))

	_, awareAfter, d := story(RevisionAware)
	add("Revision-aware: revoked user is denied on the very next check",
		"The cached ALLOW is older than doc:42's revision token, so the check falls back to the primary and returns DENY.",
		awareAfter.Decision == "DENY" && awareAfter.ServedBy == "primary" && awareAfter.CacheRev < awareAfter.RequiredRev,
		describe(awareAfter))

	again, _ := d.Check("user:alice", "doc:42")
	add("Revision-aware: the fallback refreshes the cache",
		"After one fallback the cache holds DENY at the new revision, so the next check is a fast, correct cache hit.",
		again.ServedBy == "cache" && again.Decision == "DENY" && again.CacheRev >= again.RequiredRev,
		describe(again))

	if _, err := d.Grant("user:alice", "doc:42"); err != nil {
		panic(err)
	}
	regrant, _ := d.Check("user:alice", "doc:42")
	add("Revision-aware: a re-grant is visible immediately",
		"Stale DENYs are refused too: after re-granting, the cached DENY is older than the token and Alice gets ALLOW.",
		regrant.Decision == "ALLOW" && regrant.ServedBy == "primary",
		describe(regrant))

	d2 := New(RevisionAware)
	warm, _ := d2.Check("user:bob", "doc:7")
	d2.Check("user:alice", "doc:42")
	if _, err := d2.Revoke("user:alice", "doc:42"); err != nil {
		panic(err)
	}
	other, _ := d2.Check("user:bob", "doc:7")
	add("Revision-aware: unrelated documents keep their cache hits",
		"Revoking Alice on doc:42 doesn't touch doc:7's token, so Bob's doc:7 check is still served from the cache.",
		warm.ServedBy == "primary" && other.ServedBy == "cache" && other.Correct,
		describe(other))

	return cases
}

// propertyRun replays one random workload against both modes. Writes and
// checks are generated once, so both modes see exactly the same operations.
func propertyRun(seed uint64, ops int, ps *PropertyStats) {
	rng := rand.New(rand.NewPCG(seed, 0xa11ce))
	plain, aware := New(PlainCache), New(RevisionAware)
	viewers := map[string]bool{"user:alice|doc:42": true}

	for range ops {
		u := Users[rng.IntN(len(Users))]
		doc := Docs[rng.IntN(len(Docs))]
		key := u + "|" + doc
		switch r := rng.Float64(); {
		case r < 0.05 && !viewers[key]:
			viewers[key] = true
			plain.Grant(u, doc)
			aware.Grant(u, doc)
			ps.Writes++
		case r < 0.10 && viewers[key]:
			delete(viewers, key)
			plain.Revoke(u, doc)
			aware.Revoke(u, doc)
			ps.Writes++
		default:
			for _, m := range []struct {
				d *Demo
				s *ModeStats
			}{{plain, &ps.Plain}, {aware, &ps.Aware}} {
				res, err := m.d.Check(u, doc)
				if err != nil {
					panic(err)
				}
				m.s.Checks++
				m.s.MeanMs += res.LatencyMs
				if !res.Correct {
					m.s.Wrong++
				}
				if res.Decision == "ALLOW" && res.Expected == "DENY" {
					m.s.StaleAllows++
				}
				if res.ServedBy == "cache" {
					m.s.CacheHits++
				}
				if res.ServedBy == "primary" && res.CacheRev > 0 {
					m.s.Fallbacks++
				}
			}
		}
	}
}

func propertyCases() ([]TestCase, PropertyStats) {
	ps := PropertyStats{Seeds: 50, OpsPerRun: 400}
	for seed := uint64(1); seed <= uint64(ps.Seeds); seed++ {
		propertyRun(seed, ps.OpsPerRun, &ps)
	}
	for _, s := range []*ModeStats{&ps.Plain, &ps.Aware} {
		if s.Checks > 0 {
			s.MeanMs /= float64(s.Checks)
			s.HitRatePct = 100 * float64(s.CacheHits) / float64(s.Checks)
		}
	}
	summary := func(name string, s ModeStats) string {
		return fmt.Sprintf("%s: %d checks, %d wrong (%d stale ALLOWs), cache hit rate %.1f%%, %d fallbacks, mean latency %.2f ms",
			name, s.Checks, s.Wrong, s.StaleAllows, s.HitRatePct, s.Fallbacks, s.MeanMs)
	}
	cases := []TestCase{
		{
			Name:        "Property: revision-aware decisions always match the primary",
			Kind:        "property",
			Description: fmt.Sprintf("%d random workloads × %d operations (grants, revokes, checks for %d users on %d documents). Every revision-aware decision must equal the primary's.", ps.Seeds, ps.OpsPerRun, len(Users), len(Docs)),
			Passed:      ps.Aware.Wrong == 0 && ps.Aware.Checks > 0,
			Details:     []string{summary("revision-aware", ps.Aware)},
		},
		{
			Name:        "Property sanity: the same workloads catch the plain cache",
			Kind:        "property",
			Description: "A test that can't fail proves nothing. The identical workloads must detect stale decisions in the plain cache.",
			Passed:      ps.Plain.StaleAllows > 0,
			Details:     []string{summary("plain cache", ps.Plain)},
		},
	}
	return cases, ps
}

// RunSuite runs every correctness test.
func RunSuite() SuiteResult {
	start := time.Now()
	res := SuiteResult{Cases: scenarioCases()}
	pc, ps := propertyCases()
	res.Cases = append(res.Cases, pc...)
	res.Property = ps
	res.Passed = true
	for _, c := range res.Cases {
		res.Passed = res.Passed && c.Passed
	}
	res.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	return res
}
