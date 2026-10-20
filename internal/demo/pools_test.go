package demo

import (
	"testing"

	"github.com/poorkenny/authz-lab/internal/store"
)

const carol = "user:carol"

func newPools(t *testing.T, mode, routing string) *Pools {
	t.Helper()
	p, err := NewPools(mode, routing)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// must panics on an error, which fails the test.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// drain delivers every pool until all are empty.
func drain(t *testing.T, p *Pools) {
	t.Helper()
	for p.depth() > 0 || p.inflight[0] != nil || p.inflight[1] != nil {
		for i := range NumPools {
			must(p.Deliver(i))
		}
	}
}

// outOfOrder: Carol is granted on busy instance A, revoked on idle instance
// B; B's revoke is applied first, A's grant last.
func outOfOrder(t *testing.T, p *Pools) PoolCheckResult {
	must(p.AddOtherWrites(2, 0))
	must(p.Write(store.OpTouch, carol, "doc:42", 0))
	rv := must(p.Write(store.OpDelete, carol, "doc:42", 1))
	for {
		d := must(p.Deliver(rv.Event.Pool))
		if d.Apply.Event.ID == rv.Event.ID {
			break
		}
	}
	if r := must(p.Check(carol, "doc:42")); !r.Correct {
		t.Fatalf("read right after the revoke was applied: %+v", r)
	}
	drain(t, p)
	return must(p.Check(carol, "doc:42"))
}

// race: the worker holding Carol's grant stalls before writing it; the revoke
// is applied meanwhile, then the stalled worker resumes.
func race(t *testing.T, p *Pools) PoolCheckResult {
	g := must(p.Write(store.OpTouch, carol, "doc:42", 0))
	must(p.Pick(g.Event.Pool))
	rv := must(p.Write(store.OpDelete, carol, "doc:42", 1))
	for done := false; !done; {
		d := must(p.Deliver(rv.Event.Pool))
		done = d.Apply.Event.ID == rv.Event.ID
	}
	drain(t, p)
	return must(p.Check(carol, "doc:42"))
}

func TestPools(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode, routing string
		stale         bool
	}{
		{"refresh by instance", PoolRefresh, PoolByInstance, true},
		{"refresh with revision fence by instance", PoolFenced, PoolByInstance, false},
		{"refresh partitioned by document", PoolRefresh, PoolByDocument, false},
		{"retire by instance", PoolRetire, PoolByInstance, false},
	} {
		for name, scenario := range map[string]func(*testing.T, *Pools) PoolCheckResult{"out of order": outOfOrder, "race": race} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				r := scenario(t, newPools(t, tc.mode, tc.routing))
				if r.Correct == tc.stale || r.Stuck != tc.stale {
					t.Fatalf("stale=%v want %v: %+v", !r.Correct, tc.stale, r)
				}
				if tc.stale && (r.Decision != "ALLOW" || r.Expected != "DENY") {
					t.Fatalf("want a stuck stale ALLOW, got %+v", r)
				}
			})
		}
	}
}

func TestPoolsFenceDiscards(t *testing.T) {
	p := newPools(t, PoolFenced, PoolByInstance)
	race(t, p)
	s := must(p.State())
	if len(s.History) == 0 || s.History[0].Result != "discarded" {
		t.Fatalf("want the stalled grant discarded last, got %+v", s.History)
	}
	if _, err := p.Pick(0); err != nil {
		t.Fatal(err) // empty pool: a note, not an error
	}
	if _, err := NewPools("upsert", PoolByInstance); err == nil {
		t.Fatal("want an error for an unknown mode")
	}
	if _, err := p.Write(store.OpTouch, carol, "doc:42", 2); err == nil {
		t.Fatal("want an error for an unknown instance")
	}
}
