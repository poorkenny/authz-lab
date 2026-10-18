// Package lab composes the mock system into reviewer-facing runs: the
// revocation drill (one scenario built from any combination of staleness
// sources and defenses) and the trade-off experiments.
package lab

import (
	"fmt"

	"github.com/poorkenny/authz-lab/internal/audit"
	"github.com/poorkenny/authz-lab/internal/sim"
	"github.com/poorkenny/authz-lab/internal/store"
)

type Step struct {
	T      int64            `json:"t"`
	Kind   string           `json:"kind"` // setup | advance | partition | write | content | check
	Title  string           `json:"title"`
	Detail string           `json:"detail,omitempty"`
	Rev    int64            `json:"rev,omitempty"`
	Check  *sim.CheckResult `json:"check,omitempty"`
}

type Verdict struct {
	// Outcome is "leaked", "safe" or "safe_false_deny".
	Outcome        string   `json:"outcome"`
	Leaked         bool     `json:"leaked"`
	FalseDeny      bool     `json:"falseDeny"`
	Headline       string   `json:"headline"`
	Causes         []string `json:"causes"`
	Defenses       []string `json:"defenses"`
	Tradeoffs      []string `json:"tradeoffs"`
	FinalLatencyMs float64  `json:"finalLatencyMs"`
}

type DrillResult struct {
	RunID       string          `json:"runId"`
	Config      sim.Config      `json:"config"`
	Steps       []Step          `json:"steps"`
	Final       sim.CheckResult `json:"final"`
	Control     sim.CheckResult `json:"control"`
	RevokeRev   int64           `json:"revokeRev"`
	RevokeAckMs int64           `json:"revokeAckMs"`
	Verdict     Verdict         `json:"verdict"`
	Timeline    []sim.Event     `json:"timeline"`
	State       sim.State       `json:"state"`
}

func mustTuple(s string) store.Tuple {
	t, err := store.ParseTuple(s)
	if err != nil {
		panic(err)
	}
	return t
}

// RunDrill executes the revocation drill:
//
//	setup grants → replication settles → warm-up check → [partition] →
//	revoke → [new secret content] → final check by the revoked user →
//	control check by a still-authorized user.
func RunDrill(cfg sim.Config, runID string, log *audit.Log) (*DrillResult, *sim.World, error) {
	cfg = cfg.Normalize()
	src := cfg.Sources
	w := sim.NewWorld(cfg, runID)
	if log != nil {
		w.Audit = log
	}
	r := &DrillResult{RunID: runID, Config: cfg}
	step := func(s Step) { s.T = w.Now; r.Steps = append(r.Steps, s) }
	write := func(op store.Op, tuple string) (store.Change, string, error) {
		c, tok, err := w.Write(op, mustTuple(tuple))
		return c, tok, err
	}

	setup := []string{"doc:42#owner@user:bob"}
	revoke := "doc:42#viewer@user:alice"
	if src.IndirectRevocation {
		setup = append(setup, "group:eng#member@user:alice", "folder:7#viewer@group:eng#member", "doc:42#parent@folder:7")
		revoke = "group:eng#member@user:alice"
	} else {
		setup = append(setup, "doc:42#viewer@user:alice")
	}
	var grantTok string
	for _, t := range setup {
		c, tok, err := write(store.OpTouch, t)
		if err != nil {
			return nil, nil, err
		}
		grantTok = tok
		step(Step{Kind: "write", Title: "Grant " + t, Rev: c.Rev})
	}
	w.WriteContent("doc:42", "Q3 roadmap (v1)")
	step(Step{Kind: "content", Title: "Bob writes doc:42 content v1", Detail: "token " + grantTok})

	w.Advance(10_000)
	step(Step{Kind: "advance", Title: "10 s pass: replication settles", Detail: "every replica has applied every grant"})

	warm, err := w.Check(sim.CheckRequest{Subject: "user:alice", Permission: "view", Object: "doc:42", Replica: sim.TargetReplica, Token: grantTok, Label: "warm-up"})
	if err != nil {
		return nil, nil, err
	}
	step(Step{Kind: "check", Title: "Warm-up: Alice views doc:42 via replica-2 (allowed)", Check: &warm})

	if src.Partition {
		if err := w.Partition(sim.TargetReplica); err != nil {
			return nil, nil, err
		}
		step(Step{Kind: "partition", Title: "Network partition: replica-2 cut off from changelog and fast lane"})
		w.Advance(2000)
		step(Step{Kind: "advance", Title: "2 s pass", Detail: "replica-2 keeps serving from its last snapshot"})
	}

	rc, _, err := write(store.OpDelete, revoke)
	if err != nil {
		return nil, nil, err
	}
	r.RevokeRev = rc.Rev
	r.RevokeAckMs = w.LastAckMs
	step(Step{Kind: "write", Title: "REVOKE " + revoke, Rev: rc.Rev, Detail: fmt.Sprintf("committed at rev %d, acknowledged after %d ms; affected: %v", rc.Rev, w.LastAckMs, rc.Affected)})

	finalTok := grantTok
	tokNote := "Alice's client still holds the token from before the revoke (" + grantTok + ")"
	remaining := cfg.Timing.CheckAfterRevokeMs
	if src.NewEnemy {
		gap := min(int64(50), remaining)
		w.Advance(gap)
		remaining -= gap
		c := w.WriteContent("doc:42", "Acquisition plan (v2, written after Alice's removal)")
		finalTok = c.Token
		tokNote = "content v2 carries the token taken when it was written (" + c.Token + ")"
		step(Step{Kind: "content", Title: "New enemy: Bob writes secret content v2 after the revoke", Detail: "token " + c.Token})
	}
	w.Advance(remaining)
	step(Step{Kind: "advance", Title: fmt.Sprintf("%d ms after the revoke", cfg.Timing.CheckAfterRevokeMs)})

	final, err := w.Check(sim.CheckRequest{Subject: "user:alice", Permission: "view", Object: "doc:42", Replica: sim.TargetReplica, Token: finalTok, Label: "final"})
	if err != nil {
		return nil, nil, err
	}
	step(Step{Kind: "check", Title: "Final: Alice views doc:42 via replica-2", Detail: tokNote, Check: &final})

	control, err := w.Check(sim.CheckRequest{Subject: "user:bob", Permission: "view", Object: "doc:42", Replica: sim.TargetReplica, Label: "control"})
	if err != nil {
		return nil, nil, err
	}
	step(Step{Kind: "check", Title: "Control: Bob (still the owner) views doc:42 via replica-2", Check: &control})

	r.Final, r.Control = final, control
	r.Verdict = explain(cfg, r, w)
	r.Timeline = w.Timeline()
	r.State = w.State()
	return r, w, nil
}

func explain(cfg sim.Config, r *DrillResult, w *sim.World) Verdict {
	src, d := cfg.Sources, cfg.Defenses
	f, ev := r.Final, r.Final.Evidence
	v := Verdict{Leaked: f.Stale, FalseDeny: r.Control.FalseDeny, FinalLatencyMs: ev.LatencyMs, Causes: []string{}, Defenses: []string{}, Tradeoffs: []string{}}
	rep, _ := w.Replica(sim.TargetReplica)

	if f.Stale {
		v.Outcome = "leaked"
		v.Headline = fmt.Sprintf("LEAK: Alice was ALLOWED from a snapshot at rev %d, after her access was revoked at rev %d.", ev.SnapshotRev, r.RevokeRev)
		if src.NewEnemy {
			v.Headline += " She can read content v2, written after she was removed: the \"new enemy\" problem."
		}
		switch ev.Path {
		case "cache":
			v.Causes = append(v.Causes, fmt.Sprintf("replica-2's decision cache returned an ALLOW computed at rev %d; its TTL (%d ms) had not expired, and TTL alone says nothing about revocations.", ev.SnapshotRev, src.DecisionCache.TTLMs))
			if d.Watermark && !d.RevisionKeyedCache {
				v.Causes = append(v.Causes, fmt.Sprintf("The watermark (rev %d) was known, but the cache is TTL-keyed, not revision-keyed, so the fence was never consulted.", ev.WatermarkRev))
			}
		default:
			if rep.CutAt >= 0 {
				v.Causes = append(v.Causes, fmt.Sprintf("replica-2 has been partitioned since t=%d ms: rev %d never reached it on the changelog.", rep.CutAt, r.RevokeRev))
				if d.Watermark {
					v.Causes = append(v.Causes, "The watermark can't reach it either: the fast lane is partitioned too.")
				}
				if !d.FailClosed {
					v.Causes = append(v.Causes, "Nothing told replica-2 to stop serving: fail-closed is off, so it answers confidently from its last snapshot.")
				}
			} else {
				v.Causes = append(v.Causes, fmt.Sprintf("replica-2 trails the primary by %d ms: at check time it had applied rev %d < revoke rev %d.", rep.LagMs, ev.SnapshotRev, r.RevokeRev))
			}
		}
		if src.IndirectRevocation {
			v.Causes = append(v.Causes, "The revoke removed a group membership. doc:42's own tuples never changed, so only dependency-aware fencing (group → folder → doc) can catch it.")
		}
		if d.Tokens && ev.TokenRev > 0 && ev.TokenRev < r.RevokeRev {
			v.Causes = append(v.Causes, fmt.Sprintf("The caller's token (rev %d) predates the revocation, so at_least(token) was satisfied by the stale snapshot. Tokens protect only callers whose token postdates the revoke.", ev.TokenRev))
		}
		if !d.Watermark && !d.AlwaysPrimary {
			v.Causes = append(v.Causes, "No revocation watermark: a replica has no way to learn that a revocation it hasn't applied exists.")
		}
	} else {
		v.Outcome = "safe"
		v.Headline = fmt.Sprintf("SAFE: Alice was DENIED (path: %s, %.1f ms).", ev.Path, ev.LatencyMs)
		switch ev.DefenseApplied {
		case "":
			v.Defenses = append(v.Defenses, fmt.Sprintf("No defense was needed: replica-2 had already applied the revoke (snapshot rev %d ≥ %d) before the check.", ev.SnapshotRev, r.RevokeRev))
		case "always_primary":
			v.Defenses = append(v.Defenses, "Fully consistent mode: the check read the primary, which always has the revoke.")
		case "token":
			v.Defenses = append(v.Defenses, fmt.Sprintf("Consistency token (rev %d) required a snapshot at least that fresh; replica-2 %s.", ev.TokenRev, pathPhrase(ev.Path)))
		case "watermark":
			v.Defenses = append(v.Defenses, fmt.Sprintf("Revocation watermark (rev %d) arrived on the fast lane before the changelog; replica-2 refused its stale snapshot and %s.", ev.WatermarkRev, pathPhrase(ev.Path)))
		case "fail_closed":
			v.Defenses = append(v.Defenses, "Fail-closed: replica-2 detected it could not prove freshness and stopped serving locally ("+d.FailMode+").")
		case "revision_keyed_cache":
			v.Defenses = append(v.Defenses, "Revision-keyed cache: the cached ALLOW was older than the required revision and was bypassed.")
		}
		for _, n := range ev.Notes {
			v.Defenses = append(v.Defenses, n)
		}
	}

	if r.Control.FalseDeny {
		v.Outcome = map[bool]string{true: "leaked", false: "safe_false_deny"}[f.Stale]
		v.Tradeoffs = append(v.Tradeoffs, "Availability cost: Bob is still authorized but was DENIED, because fail-closed in deny mode refuses every check on a replica that can't prove freshness.")
	}
	switch ev.Path {
	case "waited":
		v.Tradeoffs = append(v.Tradeoffs, fmt.Sprintf("Latency cost: the check waited for replica-2 to catch up (%.1f ms instead of ~%.1f ms local).", ev.LatencyMs, cfg.Timing.LocalMs))
	case "forwarded", "primary":
		v.Tradeoffs = append(v.Tradeoffs, fmt.Sprintf("Latency and load cost: the check went to the primary (%.1f ms instead of ~%.1f ms local). It's paid only by checks touching a recent revocation, except in fully consistent mode, where every check pays.", ev.LatencyMs, cfg.Timing.LocalMs))
	}
	if r.RevokeAckMs > 0 {
		v.Tradeoffs = append(v.Tradeoffs, fmt.Sprintf("Write-side cost: the revoke was acknowledged only after the watermark reached every replica that could still serve (%d ms). A partitioned replica holds the ack until its heartbeat lease expires.", r.RevokeAckMs))
	}
	if r.Control.Evidence.Path == "local" || r.Control.Evidence.Path == "cache" {
		v.Tradeoffs = append(v.Tradeoffs, fmt.Sprintf("Unaffected traffic stays fast: Bob's check was served %s in %.1f ms.", r.Control.Evidence.Path, r.Control.Evidence.LatencyMs))
	}
	return v
}

func pathPhrase(path string) string {
	switch path {
	case "waited":
		return "waited briefly to catch up"
	case "forwarded":
		return "forwarded the check to the primary"
	case "local":
		return "was already fresh enough"
	}
	return "handled it via " + path
}
