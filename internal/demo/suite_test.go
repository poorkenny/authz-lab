package demo

import "testing"

// The same suite the UI's "Run correctness tests" button executes.
func TestSuite(t *testing.T) {
	res := RunSuite()
	for _, c := range res.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if !c.Passed {
				t.Errorf("%s\n%v", c.Description, c.Details)
			}
		})
	}
}

func TestHopLatenciesAddUp(t *testing.T) {
	d := New(RevisionAware)
	for range 3 {
		r, err := d.Check("user:alice", "doc:42")
		if err != nil {
			t.Fatal(err)
		}
		var sum float64
		for _, h := range r.Hops {
			sum += h.Ms
		}
		if diff := sum - r.LatencyMs; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("hops sum to %v, latency %v", sum, r.LatencyMs)
		}
	}
}
