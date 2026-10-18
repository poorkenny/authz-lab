package lab

import (
	"testing"

	"github.com/poorkenny/authz-lab/internal/sim"
)

func drill(t *testing.T, mutate func(c *sim.Config)) *DrillResult {
	t.Helper()
	cfg := sim.DefaultConfig()
	mutate(&cfg)
	r, _, err := RunDrill(cfg, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDrillMatrix(t *testing.T) {
	lag := func(c *sim.Config) { c.Sources.ReplicationLag.Enabled = true }
	cache := func(c *sim.Config) { c.Sources.DecisionCache.Enabled = true }
	indirect := func(c *sim.Config) { c.Sources.IndirectRevocation = true }
	partition := func(c *sim.Config) { c.Sources.Partition = true }
	newEnemy := func(c *sim.Config) { c.Sources.NewEnemy = true }
	tokens := func(c *sim.Config) { c.Defenses.Tokens = true }
	watermark := func(c *sim.Config) { c.Defenses.Watermark = true }
	revCache := func(c *sim.Config) { c.Defenses.RevisionKeyedCache = true }
	failClosed := func(c *sim.Config) { c.Defenses.FailClosed = true }
	deny := func(c *sim.Config) { c.Defenses.FailMode = "deny" }
	guarded := func(c *sim.Config) { tokens(c); watermark(c); revCache(c); failClosed(c) }
	all := func(fs ...func(*sim.Config)) func(*sim.Config) {
		return func(c *sim.Config) {
			for _, f := range fs {
				f(c)
			}
		}
	}

	cases := []struct {
		name      string
		cfg       func(*sim.Config)
		leaked    bool
		falseDeny bool
	}{
		{"baseline", all(), false, false},
		{"lag leaks", lag, true, false},
		{"cache leaks", cache, true, false},
		{"indirect + lag leaks", all(indirect, lag), true, false},
		{"partition leaks", partition, true, false},
		{"new enemy leaks", all(lag, newEnemy), true, false},
		{"tokens fix new enemy", all(lag, newEnemy, tokens), false, false},
		{"tokens alone miss old-token callers", all(lag, tokens), true, false},
		{"watermark fixes indirect lag", all(indirect, lag, watermark), false, false},
		{"watermark without revision cache still leaks", all(cache, watermark), true, false},
		{"watermark + revision cache fixes cache", all(cache, watermark, revCache), false, false},
		{"watermark alone misses partition", all(partition, watermark), true, false},
		{"fail-closed forward fixes partition", all(partition, watermark, failClosed), false, false},
		{"fail-closed deny costs availability", all(partition, watermark, failClosed, deny), false, true},
		{"guarded vs everything", all(lag, cache, indirect, partition, newEnemy, guarded), false, false},
		{"always primary", all(lag, cache, partition, func(c *sim.Config) { c.Defenses.AlwaysPrimary = true }), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := drill(t, tc.cfg)
			if r.Verdict.Leaked != tc.leaked {
				t.Errorf("leaked = %v, want %v (%s; final %+v)", r.Verdict.Leaked, tc.leaked, r.Verdict.Headline, r.Final.Evidence)
			}
			if r.Verdict.FalseDeny != tc.falseDeny {
				t.Errorf("falseDeny = %v, want %v", r.Verdict.FalseDeny, tc.falseDeny)
			}
			if r.Final.Decision == "ALLOW" == !r.Final.Stale && r.Final.Decision == "ALLOW" {
				t.Errorf("revoked user allowed without stale flag")
			}
		})
	}
}

// The safety invariant: under the guarded preset no randomized workload ever
// produces an ALLOW the primary would deny.
func TestGuardedNeverServesStaleAllow(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		res, err := RunLagSweep(SweepRequest{Modes: []string{"guarded", "full"}, OpsPerPoint: 1500, RevokeRate: 0.08, Seed: seed})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range res.Points {
			if p.StaleAllows != 0 {
				t.Fatalf("seed %d: %s at lag %d ms served %d stale allows", seed, p.Mode, p.LagMs, p.StaleAllows)
			}
		}
	}
}

// And the naive system really is broken, otherwise the lab proves nothing.
func TestNaiveLeaksUnderLag(t *testing.T) {
	res, err := RunLagSweep(SweepRequest{Modes: []string{"naive"}, LagsMs: []int64{2000}, OpsPerPoint: 1500, RevokeRate: 0.08})
	if err != nil {
		t.Fatal(err)
	}
	if res.Points[0].StaleAllows == 0 {
		t.Fatal("naive mode produced no stale allows at 2 s lag")
	}
}
