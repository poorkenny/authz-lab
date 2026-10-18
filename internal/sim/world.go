// Package sim is the mock distributed authorization system: a primary store,
// a changelog and a watermark fast lane feeding N check replicas, a content
// store, and the audit log. Time is simulated (milliseconds) so every run is
// deterministic and can be replayed step by step in the UI.
package sim

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/poorkenny/authz-lab/internal/audit"
	"github.com/poorkenny/authz-lab/internal/engine"
	"github.com/poorkenny/authz-lab/internal/store"
)

// TargetReplica is the replica the staleness sources act on.
const TargetReplica = "replica-2"

type World struct {
	Now      int64
	RunID    string
	Store    *store.Store
	Replicas []*Replica
	Content  map[string]Content
	Audit    *audit.Log
	// LastAckMs is how long the last write waited before being acknowledged.
	LastAckMs int64

	cfg    Config
	events []Event
	seq    int
	nextID int
}

type Content struct {
	Object  string `json:"object"`
	Version int    `json:"version"`
	Body    string `json:"body"`
	// Token is the consistency token taken when the content was written
	// (Zanzibar stores a zookie alongside each content version).
	Token string `json:"token"`
	At    int64  `json:"at"`
}

// Event is one step of data flow between components, used to animate the diagram.
type Event struct {
	Seq      int    `json:"seq"`
	T        int64  `json:"t"`
	Kind     string `json:"kind"` // write | replicate | watermark | blocked | check | route | decision | audit | content | partition
	From     string `json:"from"`
	To       string `json:"to"`
	Label    string `json:"label"`
	Rev      int64  `json:"rev,omitempty"`
	Severity string `json:"severity,omitempty"` // info | ok | warn | danger
	CheckID  string `json:"checkId,omitempty"`
	prio     int
}

func NewWorld(cfg Config, runID string) *World {
	cfg = cfg.Normalize()
	target := cfg.Timing.BaseLagMs
	if cfg.Sources.ReplicationLag.Enabled {
		target = cfg.Sources.ReplicationLag.LagMs
	}
	return &World{
		RunID: runID,
		Store: store.New(),
		Replicas: []*Replica{
			newReplica("replica-1", cfg.Timing.BaseLagMs),
			newReplica(TargetReplica, target),
			newReplica("replica-3", cfg.Timing.BaseLagMs),
		},
		Content: map[string]Content{},
		Audit:   &audit.Log{},
		cfg:     cfg,
	}
}

func (w *World) Config() Config { return w.cfg }

// SetConfig changes defenses/timing on a live world; replica lag follows the
// replication-lag source.
func (w *World) SetConfig(cfg Config) {
	w.cfg = cfg.Normalize()
	for _, r := range w.Replicas {
		r.LagMs = w.cfg.Timing.BaseLagMs
		if r.ID == TargetReplica && w.cfg.Sources.ReplicationLag.Enabled {
			r.LagMs = w.cfg.Sources.ReplicationLag.LagMs
		}
	}
}

func (w *World) Replica(id string) (*Replica, error) {
	if id == "" {
		id = "replica-1"
	}
	for _, r := range w.Replicas {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, fmt.Errorf("unknown replica %q", id)
}

func (w *World) Advance(ms int64) {
	if ms > 0 {
		w.Now += ms
	}
}

func (w *World) id(prefix string) string {
	w.nextID++
	return fmt.Sprintf("%s-%s%d", prefix, w.RunID, w.nextID)
}

func (w *World) emit(e Event) {
	w.seq++
	e.Seq = w.seq
	e.T = w.Now
	e.prio = 1
	if len(w.events) < 20000 {
		w.events = append(w.events, e)
	}
}

func Token(rev int64) string { return "zk_" + strconv.FormatInt(rev, 10) }

func ParseToken(tok string) (int64, error) {
	if tok == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(tok, "zk_"), 10, 64)
	if err != nil || !strings.HasPrefix(tok, "zk_") || n < 0 {
		return 0, fmt.Errorf("invalid consistency token %q", tok)
	}
	return n, nil
}

// Write commits a tuple change at the primary and returns its changelog entry
// and consistency token.
func (w *World) Write(op store.Op, t store.Tuple) (store.Change, string, error) {
	c, err := w.Store.Write(op, t, w.Now)
	if err != nil {
		return store.Change{}, "", err
	}
	tok := Token(c.Rev)
	w.emit(Event{Kind: "write", From: "client", To: "writeapi", Label: fmt.Sprintf("%s %s", op, t), Rev: c.Rev})
	sev := "info"
	if c.Revoke {
		sev = "warn"
	}
	w.emit(Event{Kind: "write", From: "writeapi", To: "primary", Label: fmt.Sprintf("commit rev %d", c.Rev), Rev: c.Rev, Severity: sev})
	w.emit(Event{Kind: "write", From: "primary", To: "changelog", Label: fmt.Sprintf("append rev %d", c.Rev), Rev: c.Rev})
	if c.Revoke {
		w.emit(Event{Kind: "watermark", From: "primary", To: "fastlane", Label: fmt.Sprintf("revoke watermark rev %d → %s", c.Rev, strings.Join(c.Affected, ", ")), Rev: c.Rev, Severity: "warn"})
	}
	// With the watermark defense a revocation is acknowledged only once the
	// fence has reached every replica that could still serve: connected
	// replicas after one fast-lane hop, partitioned ones once their
	// heartbeat lease has expired (fail-closed makes them stop serving).
	// Without fail-closed a partitioned replica is simply not waited for.
	w.LastAckMs = 0
	if c.Revoke && w.cfg.Defenses.Watermark {
		wait := w.cfg.Timing.FastLaneMs
		if w.cfg.Defenses.FailClosed {
			for _, r := range w.Replicas {
				if r.CutAt >= 0 {
					wait = max(wait, r.CutAt+w.cfg.Defenses.HeartbeatTimeoutMs-w.Now)
				}
			}
		}
		w.Now += wait
		w.LastAckMs = wait
	}
	ackLabel := fmt.Sprintf("ack rev %d, token %s", c.Rev, tok)
	if w.LastAckMs > 0 {
		ackLabel = fmt.Sprintf("revoke acknowledged after watermark fan-out (%d ms), token %s", w.LastAckMs, tok)
	}
	w.emit(Event{Kind: "write", From: "writeapi", To: "client", Label: ackLabel, Rev: c.Rev, Severity: sev})
	w.emit(Event{Kind: "audit", From: "writeapi", To: "audit", Label: fmt.Sprintf("audit write rev %d", c.Rev), Rev: c.Rev})
	w.Audit.Append(audit.Record{
		ID: w.id("wr"), RunID: w.RunID, T: w.Now, Kind: "write",
		Tuple: t.String(), Op: string(op), Rev: c.Rev, PrimaryRev: c.Rev, LatencyMs: float64(w.LastAckMs),
	})
	return c, tok, nil
}

// WriteContent stores a new content version (in a separate, always-fresh
// content store) together with a consistency token taken at write time.
func (w *World) WriteContent(object, body string) Content {
	prev := w.Content[object]
	c := Content{Object: object, Version: prev.Version + 1, Body: body, Token: Token(w.Store.Rev()), At: w.Now}
	w.Content[object] = c
	w.emit(Event{Kind: "content", From: "client", To: "content", Label: fmt.Sprintf("write %s content v%d (token %s)", object, c.Version, c.Token), Severity: "warn"})
	return c
}

// Partition cuts a replica off from the changelog and the fast lane.
func (w *World) Partition(id string) error {
	r, err := w.Replica(id)
	if err != nil {
		return err
	}
	if r.CutAt < 0 {
		r.CutAt = w.Now
		w.emit(Event{Kind: "partition", From: "changelog", To: r.ID, Label: r.ID + " partitioned from changelog + fast lane", Severity: "danger"})
	}
	return nil
}

// Heal reconnects a partitioned replica. Delivery resumes as if the backlog
// arrives immediately.
func (w *World) Heal(id string) error {
	r, err := w.Replica(id)
	if err != nil {
		return err
	}
	r.CutAt = -1
	return nil
}

type CheckRequest struct {
	Subject    string `json:"subject"`
	Permission string `json:"permission"`
	Object     string `json:"object"`
	Replica    string `json:"replica,omitempty"`
	Token      string `json:"token,omitempty"`
	// Label annotates the check in the timeline (e.g. "final check").
	Label string `json:"label,omitempty"`
}

type Evidence struct {
	ServedBy          string   `json:"servedBy"`
	SnapshotRev       int64    `json:"snapshotRev"`
	RequiredRev       int64    `json:"requiredRev"`
	TokenRev          int64    `json:"tokenRev"`
	WatermarkRev      int64    `json:"watermarkRev"`
	PrimaryRevAtStart int64    `json:"primaryRevAtStart"`
	Path              string   `json:"path"` // primary | local | cache | waited | forwarded | fail_closed
	LatencyMs         float64  `json:"latencyMs"`
	DefenseApplied    string   `json:"defenseApplied,omitempty"`
	Resolution        []string `json:"resolution"`
	Notes             []string `json:"notes"`
}

type CheckResult struct {
	CheckID        string   `json:"checkId"`
	Subject        string   `json:"subject"`
	Permission     string   `json:"permission"`
	Object         string   `json:"object"`
	Decision       string   `json:"decision"`
	OracleDecision string   `json:"oracleDecision"`
	Stale          bool     `json:"stale"`     // ALLOW that the primary would DENY: security failure
	FalseDeny      bool     `json:"falseDeny"` // DENY that the primary would ALLOW: availability cost
	Evidence       Evidence `json:"evidence"`
	T              int64    `json:"t"`
}

func decision(allow bool) string {
	if allow {
		return "ALLOW"
	}
	return "DENY"
}

// Check runs one permission check through the configured path.
func (w *World) Check(req CheckRequest) (CheckResult, error) {
	d := w.cfg.Defenses
	tm := w.cfg.Timing
	now := w.Now
	changes := w.Store.Changes()
	primaryRev := w.Store.Rev()

	rep, err := w.Replica(req.Replica)
	if err != nil {
		return CheckResult{}, err
	}
	tokenRev, err := ParseToken(req.Token)
	if err != nil {
		return CheckResult{}, err
	}
	oracleAllow, _, err := engine.Check(w.Store, primaryRev, req.Subject, req.Permission, req.Object)
	if err != nil {
		return CheckResult{}, err
	}

	res := CheckResult{CheckID: w.id("chk"), Subject: req.Subject, Permission: req.Permission, Object: req.Object, T: now}
	ev := Evidence{ServedBy: rep.ID, PrimaryRevAtStart: primaryRev, TokenRev: tokenRev, Notes: []string{}}
	desc := fmt.Sprintf("check %s %s %s", req.Subject, req.Permission, req.Object)
	if req.Label != "" {
		desc = req.Label + ": " + desc
	}
	if req.Token != "" {
		desc += " token=" + req.Token
	}
	w.emit(Event{Kind: "check", From: "client", To: "checkapi", Label: desc, CheckID: res.CheckID})

	var allow bool
	evalAt := func(rev int64) {
		allow, ev.Resolution, _ = engine.Check(w.Store, rev, req.Subject, req.Permission, req.Object)
		ev.SnapshotRev = rev
	}
	toPrimary := func(extraMs float64) {
		evalAt(primaryRev)
		ev.ServedBy = "primary"
		ev.LatencyMs = extraMs + tm.PrimaryRTTMs
	}
	key := req.Subject + "|" + req.Permission + "|" + req.Object
	cacheOn := w.cfg.Sources.DecisionCache.Enabled

	switch {
	case d.AlwaysPrimary:
		ev.Path = "primary"
		ev.DefenseApplied = "always_primary"
		toPrimary(0)
		ev.Notes = append(ev.Notes, "fully consistent mode: every check reads the primary")
	default:
		applied := rep.AppliedRev(changes, now)
		var required int64
		if d.Tokens && tokenRev > 0 {
			required = tokenRev
			ev.Notes = append(ev.Notes, fmt.Sprintf("token requires snapshot ≥ rev %d", tokenRev))
		}
		if d.Watermark {
			ev.WatermarkRev = rep.Watermark(changes, now, tm.FastLaneMs, req.Object, d.WatermarkGranularity)
			if ev.WatermarkRev > 0 {
				ev.Notes = append(ev.Notes, fmt.Sprintf("revocation watermark for %s at rev %d", req.Object, ev.WatermarkRev))
			}
			required = max(required, ev.WatermarkRev)
		}
		ev.RequiredRev = required
		fence := "token"
		if ev.WatermarkRev > tokenRev || !d.Tokens {
			fence = "watermark"
		}

		failReason := ""
		if d.FailClosed {
			if age := rep.HeartbeatAge(now); age > d.HeartbeatTimeoutMs {
				failReason = fmt.Sprintf("fast-lane heartbeat lost for %d ms (> %d ms)", age, d.HeartbeatTimeoutMs)
			} else if lag := rep.ObservedLag(changes, now, tm.FastLaneMs); lag > d.LagBoundMs {
				failReason = fmt.Sprintf("observed replication lag %d ms (> %d ms bound)", lag, d.LagBoundMs)
			}
		}

		cached, hit := rep.cache[key]
		hit = cacheOn && hit && cached.expires > now

		switch {
		case failReason != "":
			ev.DefenseApplied = "fail_closed"
			ev.Notes = append(ev.Notes, "fail-closed: "+failReason)
			if d.FailMode == "deny" {
				ev.Path = "fail_closed"
				allow, ev.Resolution, ev.SnapshotRev = false, nil, applied
				ev.LatencyMs = tm.LocalMs
			} else {
				ev.Path = "forwarded"
				toPrimary(0)
			}
		case hit && (!d.RevisionKeyedCache || cached.rev >= required):
			ev.Path = "cache"
			allow, ev.Resolution, ev.SnapshotRev = cached.allow, cached.trace, cached.rev
			ev.LatencyMs = tm.CacheMs
			ev.Notes = append(ev.Notes, fmt.Sprintf("decision cache hit (computed at rev %d, expires in %d ms)", cached.rev, cached.expires-now))
		default:
			if hit {
				ev.DefenseApplied = "revision_keyed_cache"
				ev.Notes = append(ev.Notes, fmt.Sprintf("cache entry at rev %d is below required rev %d: bypassed", cached.rev, required))
			}
			if applied >= required {
				ev.Path = "local"
				evalAt(applied)
				ev.LatencyMs = tm.LocalMs
			} else if at := rep.CatchUpAt(changes, required); at >= 0 && at-now <= d.WaitBudgetMs {
				ev.Path = "waited"
				ev.DefenseApplied = fence
				evalAt(rep.AppliedRev(changes, at))
				ev.LatencyMs = float64(at-now) + tm.LocalMs
				ev.Notes = append(ev.Notes, fmt.Sprintf("replica at rev %d < required %d: waited %d ms to catch up", applied, required, at-now))
			} else {
				ev.Path = "forwarded"
				ev.DefenseApplied = fence
				toPrimary(float64(d.WaitBudgetMs))
				ev.Notes = append(ev.Notes, fmt.Sprintf("replica at rev %d < required %d and won't catch up within %d ms: forwarded to primary", applied, required, d.WaitBudgetMs))
			}
		}
	}

	if cacheOn && ev.Path != "cache" && ev.Path != "fail_closed" {
		rep.cache[key] = cacheEntry{allow: allow, rev: ev.SnapshotRev, expires: now + w.cfg.Sources.DecisionCache.TTLMs, trace: ev.Resolution}
	}
	if ev.Resolution == nil {
		ev.Resolution = []string{}
	}

	res.Decision = decision(allow)
	res.OracleDecision = decision(oracleAllow)
	res.Stale = allow && !oracleAllow
	res.FalseDeny = !allow && oracleAllow
	res.Evidence = ev

	node := ev.ServedBy
	routeLabel := map[string]string{
		"primary": "read primary", "local": "local snapshot", "cache": "decision cache",
		"waited": "wait for catch-up", "forwarded": "forward to primary", "fail_closed": "fail closed",
	}[ev.Path]
	w.emit(Event{Kind: "route", From: "checkapi", To: node, Label: fmt.Sprintf("%s (snapshot rev %d, required %d)", routeLabel, ev.SnapshotRev, ev.RequiredRev), Rev: ev.SnapshotRev, CheckID: res.CheckID})
	sev := "ok"
	switch {
	case res.Stale:
		sev = "danger"
	case res.FalseDeny:
		sev = "warn"
	}
	verdict := ""
	if res.Stale {
		verdict = fmt.Sprintf(" STALE: primary is at rev %d and says DENY", primaryRev)
	} else if res.FalseDeny {
		verdict = " (false deny: primary says ALLOW)"
	}
	w.emit(Event{Kind: "decision", From: "checkapi", To: "client", Label: fmt.Sprintf("%s @ rev %d in %.1f ms%s", res.Decision, ev.SnapshotRev, ev.LatencyMs, verdict), Rev: ev.SnapshotRev, Severity: sev, CheckID: res.CheckID})
	w.emit(Event{Kind: "audit", From: "checkapi", To: "audit", Label: "audit " + res.CheckID, CheckID: res.CheckID})

	w.Audit.Append(audit.Record{
		ID: res.CheckID, RunID: w.RunID, T: now, Kind: "check",
		Subject: req.Subject, Permission: req.Permission, Object: req.Object,
		Decision: res.Decision, OracleDecision: res.OracleDecision, Stale: res.Stale, FalseDeny: res.FalseDeny,
		ServedBy: ev.ServedBy, SnapshotRev: ev.SnapshotRev, RequiredRev: ev.RequiredRev, WatermarkRev: ev.WatermarkRev,
		PrimaryRev: primaryRev, Path: ev.Path, LatencyMs: ev.LatencyMs, DefenseApplied: ev.DefenseApplied, Notes: ev.Notes,
	})
	return res, nil
}

// Timeline merges recorded events with changelog / fast-lane deliveries up to
// the current time, ordered for replay.
func (w *World) Timeline() []Event {
	out := append([]Event(nil), w.events...)
	add := func(e Event) {
		e.prio = 0
		out = append(out, e)
	}
	fast := w.cfg.Timing.FastLaneMs
	for _, c := range w.Store.Changes() {
		for _, r := range w.Replicas {
			arrival := c.At + r.LagMs
			switch {
			case r.CutAt >= 0 && arrival >= r.CutAt:
				if r.CutAt <= w.Now {
					add(Event{T: max(r.CutAt, c.At), Kind: "blocked", From: "changelog", To: r.ID, Label: fmt.Sprintf("rev %d never arrives (partitioned)", c.Rev), Rev: c.Rev, Severity: "danger"})
				}
			case arrival <= w.Now:
				sev := "info"
				if c.Revoke {
					sev = "ok"
				}
				add(Event{T: arrival, Kind: "replicate", From: "changelog", To: r.ID, Label: fmt.Sprintf("apply rev %d (%s %s)", c.Rev, c.Op, c.Tuple), Rev: c.Rev, Severity: sev})
			}
			if !c.Revoke {
				continue
			}
			wa := c.At + fast
			switch {
			case r.CutAt >= 0 && wa >= r.CutAt:
				if r.CutAt <= w.Now {
					add(Event{T: max(r.CutAt, c.At), Kind: "blocked", From: "fastlane", To: r.ID, Label: fmt.Sprintf("watermark rev %d never arrives (partitioned)", c.Rev), Rev: c.Rev, Severity: "danger"})
				}
			case wa <= w.Now:
				add(Event{T: wa, Kind: "watermark", From: "fastlane", To: r.ID, Label: fmt.Sprintf("watermark rev %d for %s", c.Rev, strings.Join(c.Affected, ", ")), Rev: c.Rev, Severity: "ok"})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].T != out[j].T {
			return out[i].T < out[j].T
		}
		if out[i].prio != out[j].prio {
			return out[i].prio < out[j].prio
		}
		return out[i].Seq < out[j].Seq
	})
	for i := range out {
		out[i].Seq = i + 1
	}
	return out
}

type State struct {
	Now        int64              `json:"now"`
	PrimaryRev int64              `json:"primaryRev"`
	Replicas   []ReplicaState     `json:"replicas"`
	Content    map[string]Content `json:"content"`
	Config     Config             `json:"config"`
}

func (w *World) State() State {
	s := State{Now: w.Now, PrimaryRev: w.Store.Rev(), Content: w.Content, Config: w.cfg}
	for _, r := range w.Replicas {
		s.Replicas = append(s.Replicas, ReplicaState{
			ID: r.ID, LagMs: r.LagMs, AppliedRev: r.AppliedRev(w.Store.Changes(), w.Now),
			Partitioned: r.CutAt >= 0, HeartbeatAge: r.HeartbeatAge(w.Now), CacheEntries: len(r.cache),
		})
	}
	return s
}
