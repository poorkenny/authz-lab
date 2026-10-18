package sim

import (
	"strings"

	"github.com/poorkenny/authz-lab/internal/store"
)

// Replica is a check replica: a snapshot of the primary that trails it by
// LagMs on the changelog, plus a per-replica decision cache. Delivery is
// computed from simulated time, so the replica's state at any instant is
// deterministic.
type Replica struct {
	ID    string `json:"id"`
	LagMs int64  `json:"lagMs"`
	// CutAt is the simulated time the replica was partitioned; -1 if connected.
	CutAt int64 `json:"cutAt"`

	cache map[string]cacheEntry
}

type cacheEntry struct {
	allow   bool
	rev     int64
	expires int64
	trace   []string
}

func newReplica(id string, lag int64) *Replica {
	return &Replica{ID: id, LagMs: lag, CutAt: -1, cache: map[string]cacheEntry{}}
}

func (r *Replica) delivered(arrival, now int64) bool {
	return arrival <= now && (r.CutAt < 0 || arrival < r.CutAt)
}

// AppliedRev is the highest revision the replica has applied by `now`
// (changelog delivery is in order).
func (r *Replica) AppliedRev(changes []store.Change, now int64) int64 {
	var rev int64
	for _, c := range changes {
		if !r.delivered(c.At+r.LagMs, now) {
			break
		}
		rev = c.Rev
	}
	return rev
}

// CatchUpAt returns when the replica will have applied rev, or -1 if never
// (partitioned).
func (r *Replica) CatchUpAt(changes []store.Change, rev int64) int64 {
	for _, c := range changes {
		if c.Rev == rev {
			arrival := c.At + r.LagMs
			if r.CutAt >= 0 && arrival >= r.CutAt {
				return -1
			}
			return arrival
		}
	}
	return -1
}

// Watermark is the highest revocation revision received on the fast lane that
// covers object.
func (r *Replica) Watermark(changes []store.Change, now, fastLane int64, object, granularity string) int64 {
	var wm int64
	ns, _, _ := strings.Cut(object, ":")
	for _, c := range changes {
		if !c.Revoke || !r.delivered(c.At+fastLane, now) {
			continue
		}
		for _, a := range c.Affected {
			if a == object || granularity == "namespace" && strings.HasPrefix(a, ns+":") {
				wm = max(wm, c.Rev)
				break
			}
		}
	}
	return wm
}

// HeartbeatAge is how long since the replica last heard the fast lane.
func (r *Replica) HeartbeatAge(now int64) int64 {
	if r.CutAt < 0 || now < r.CutAt {
		return 0
	}
	return now - r.CutAt
}

// ObservedLag is the replica's own view of how far behind it is: the age of
// the oldest change it has learned about (via fast-lane heartbeats carrying
// the primary revision) but not yet applied.
func (r *Replica) ObservedLag(changes []store.Change, now, fastLane int64) int64 {
	for _, c := range changes {
		if r.delivered(c.At+r.LagMs, now) {
			continue
		}
		if r.delivered(c.At+fastLane, now) {
			return now - c.At
		}
		return 0
	}
	return 0
}

// State is the replica snapshot reported to the UI.
type ReplicaState struct {
	ID           string `json:"id"`
	LagMs        int64  `json:"lagMs"`
	AppliedRev   int64  `json:"appliedRev"`
	Partitioned  bool   `json:"partitioned"`
	HeartbeatAge int64  `json:"heartbeatAgeMs"`
	CacheEntries int    `json:"cacheEntries"`
}
