package lab

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/poorkenny/authz-lab/internal/sim"
	"github.com/poorkenny/authz-lab/internal/store"
)

// SweepRequest drives the lag-sweep experiment: the same randomized workload
// replayed under each consistency preset at each replication lag.
type SweepRequest struct {
	LagsMs      []int64  `json:"lagsMs"`
	Modes       []string `json:"modes"`
	OpsPerPoint int      `json:"opsPerPoint"`
	RevokeRate  float64  `json:"revokeRate"` // fraction of ops that are revocations
	Seed        uint64   `json:"seed"`
}

type SweepPoint struct {
	Mode           string         `json:"mode"`
	LagMs          int64          `json:"lagMs"`
	Checks         int            `json:"checks"`
	P50            float64        `json:"p50Ms"`
	P99            float64        `json:"p99Ms"`
	Mean           float64        `json:"meanMs"`
	Paths          map[string]int `json:"paths"`
	LocalPct       float64        `json:"localPct"`
	PrimaryLoadPct float64        `json:"primaryLoadPct"`
	StaleAllows    int            `json:"staleAllows"`
	FalseDenies    int            `json:"falseDenies"`
	RevokeAckMs    float64        `json:"revokeAckMs"` // mean revocation acknowledgment latency
}

type SweepResult struct {
	Params SweepRequest `json:"params"`
	Points []SweepPoint `json:"points"`
}

func (r SweepRequest) normalize() (SweepRequest, error) {
	if len(r.LagsMs) == 0 {
		r.LagsMs = []int64{0, 100, 250, 500, 1000, 2000, 5000}
	}
	if len(r.LagsMs) > 10 {
		return r, fmt.Errorf("at most 10 lag values")
	}
	for i, l := range r.LagsMs {
		r.LagsMs[i] = min(max(l, 0), 30000)
	}
	if len(r.Modes) == 0 {
		r.Modes = sim.PresetNames
	}
	for _, m := range r.Modes {
		ok := false
		for _, p := range sim.PresetNames {
			ok = ok || m == p
		}
		if !ok {
			return r, fmt.Errorf("unknown mode %q", m)
		}
	}
	if r.OpsPerPoint <= 0 {
		r.OpsPerPoint = 1000
	}
	r.OpsPerPoint = min(r.OpsPerPoint, 3000)
	if r.RevokeRate <= 0 {
		r.RevokeRate = 0.03
	}
	r.RevokeRate = min(r.RevokeRate, 0.3)
	if r.Seed == 0 {
		r.Seed = 42
	}
	return r, nil
}

func RunLagSweep(req SweepRequest) (*SweepResult, error) {
	req, err := req.normalize()
	if err != nil {
		return nil, err
	}
	res := &SweepResult{Params: req}
	for _, mode := range req.Modes {
		for _, lag := range req.LagsMs {
			p, err := runPoint(mode, lag, req)
			if err != nil {
				return nil, err
			}
			res.Points = append(res.Points, p)
		}
	}
	return res, nil
}

type pair struct{ user, doc string }

// runPoint replays one workload. The RNG sequence depends only on the seed and
// the lag, so every mode sees exactly the same writes and checks.
func runPoint(mode string, lag int64, req SweepRequest) (SweepPoint, error) {
	base := sim.DefaultConfig()
	base.Sources.DecisionCache = sim.CacheSource{Enabled: true, TTLMs: 1000}
	cfg := sim.Preset(mode, base)
	w := sim.NewWorld(cfg, "sweep")
	for _, r := range w.Replicas {
		r.LagMs = lag
	}
	rng := rand.New(rand.NewPCG(req.Seed, uint64(lag)))

	const users, docs = 20, 30
	grants := map[pair]bool{}
	var grantList []pair
	docTok := map[string]string{}
	grant := func(p pair) error {
		if grants[p] {
			return nil
		}
		_, tok, err := w.Write(store.OpTouch, store.Tuple{Object: p.doc, Relation: "viewer", Subject: p.user})
		if err != nil {
			return err
		}
		grants[p] = true
		grantList = append(grantList, p)
		docTok[p.doc] = tok
		return nil
	}
	randPair := func() pair {
		return pair{fmt.Sprintf("user:u%d", rng.IntN(users)), fmt.Sprintf("doc:d%d", rng.IntN(docs))}
	}
	for range docs * 4 {
		if err := grant(randPair()); err != nil {
			return SweepPoint{}, err
		}
	}
	w.Advance(10_000)

	type revoked struct {
		p  pair
		at int64
	}
	var recent []revoked
	var lat []float64
	var ackSum float64
	var revokes int
	pt := SweepPoint{Mode: mode, LagMs: lag, Paths: map[string]int{}}

	for range req.OpsPerPoint {
		w.Advance(5)
		r := rng.Float64()
		switch {
		case r < req.RevokeRate && len(grantList) > 0:
			i := rng.IntN(len(grantList))
			p := grantList[i]
			grantList[i] = grantList[len(grantList)-1]
			grantList = grantList[:len(grantList)-1]
			delete(grants, p)
			_, tok, err := w.Write(store.OpDelete, store.Tuple{Object: p.doc, Relation: "viewer", Subject: p.user})
			if err != nil {
				return SweepPoint{}, err
			}
			docTok[p.doc] = tok
			ackSum += float64(w.LastAckMs)
			revokes++
			recent = append(recent, revoked{p, w.Now})
			if len(recent) > 50 {
				recent = recent[1:]
			}
		case r < 2*req.RevokeRate:
			if err := grant(randPair()); err != nil {
				return SweepPoint{}, err
			}
		default:
			// 30% of checks come from recently revoked users trying again;
			// the rest is ordinary traffic from users who still have access.
			var p pair
			switch {
			case len(recent) > 0 && rng.Float64() < 0.3:
				p = recent[rng.IntN(len(recent))].p
			case len(grantList) > 0:
				p = grantList[rng.IntN(len(grantList))]
			default:
				p = randPair()
			}
			tok := ""
			if rng.Float64() < 0.5 {
				tok = docTok[p.doc]
			}
			res, err := w.Check(sim.CheckRequest{Subject: p.user, Permission: "view", Object: p.doc, Replica: fmt.Sprintf("replica-%d", rng.IntN(3)+1), Token: tok})
			if err != nil {
				return SweepPoint{}, err
			}
			ev := res.Evidence
			lat = append(lat, ev.LatencyMs)
			pt.Paths[ev.Path]++
			if res.Stale {
				pt.StaleAllows++
			}
			if res.FalseDeny {
				pt.FalseDenies++
			}
		}
	}

	if revokes > 0 {
		pt.RevokeAckMs = ackSum / float64(revokes)
	}
	pt.Checks = len(lat)
	if pt.Checks > 0 {
		sort.Float64s(lat)
		var sum float64
		for _, l := range lat {
			sum += l
		}
		pt.Mean = sum / float64(pt.Checks)
		pt.P50 = lat[pt.Checks/2]
		pt.P99 = lat[min(pt.Checks-1, pt.Checks*99/100)]
		n := float64(pt.Checks)
		pt.LocalPct = 100 * float64(pt.Paths["local"]+pt.Paths["cache"]) / n
		pt.PrimaryLoadPct = 100 * float64(pt.Paths["primary"]+pt.Paths["forwarded"]) / n
	}
	return pt, nil
}
