package demo

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Limits for an uploaded log.
const (
	MaxReplayEvents       = 20000
	DefaultInvalidationMs = 1000
	MaxInvalidationMs     = 3_600_000
	maxReplayIssues       = 20
)

// LogEvent is one line of an uploaded log: a permission change ("grant" or
// "revoke") or a permission check ("check"), at time T. A check may carry
// the decision the production system returned, to evaluate it too.
type LogEvent struct {
	T        time.Time
	Event    string
	Subject  string
	Object   string
	Decision string // "", "ALLOW" or "DENY"
	Line     int
}

// ParseLog reads a log in JSON Lines or CSV. The format is detected from the
// first non-blank character: '{' means JSON Lines.
//
//	{"ts":"2026-10-18T10:00:00Z","event":"revoke","subject":"user:alice","object":"doc:42"}
//	ts,event,subject,object,decision
//
// ts is an RFC 3339 timestamp or a number of seconds (fractions allowed).
// Bare IDs get the user: / doc: prefix. Malformed lines are skipped and
// reported in issues (at most maxReplayIssues are listed).
func ParseLog(text string) (events []LogEvent, issues []string, skipped int, err error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, nil, 0, fmt.Errorf("the log is empty")
	}
	type raw struct {
		line                                 int
		ts, event, subject, object, decision string
	}
	var rows []raw
	if trimmed[0] == '{' {
		sc := bufio.NewScanner(strings.NewReader(text))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for n := 1; sc.Scan(); n++ {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				rows = append(rows, raw{line: n, event: "\x00invalid JSON"})
				continue
			}
			str := func(keys ...string) string {
				for _, k := range keys {
					switch v := m[k].(type) {
					case string:
						return v
					case float64:
						return strconv.FormatFloat(v, 'f', -1, 64)
					}
				}
				return ""
			}
			rows = append(rows, raw{n, str("ts", "time", "timestamp"), str("event", "type"), str("subject", "user"), str("object", "resource", "doc"), str("decision")})
		}
		if err := sc.Err(); err != nil {
			return nil, nil, 0, err
		}
	} else {
		r := csv.NewReader(strings.NewReader(text))
		r.FieldsPerRecord = -1
		r.TrimLeadingSpace = true
		header, err := r.Read()
		if err != nil {
			return nil, nil, 0, fmt.Errorf("read the CSV header: %w", err)
		}
		col := map[string]int{}
		for i, h := range header {
			col[strings.ToLower(strings.TrimSpace(h))] = i
		}
		for _, need := range []string{"ts", "event", "subject", "object"} {
			if _, ok := col[need]; !ok {
				return nil, nil, 0, fmt.Errorf("the CSV header must have the columns ts,event,subject,object (and optionally decision); missing %q", need)
			}
		}
		for {
			rec, err := r.Read()
			if err == io.EOF {
				break
			}
			line, _ := r.FieldPos(0)
			if err != nil {
				rows = append(rows, raw{line: line, event: "\x00invalid CSV"})
				continue
			}
			get := func(k string) string {
				if i, ok := col[k]; ok && i < len(rec) {
					return strings.TrimSpace(rec[i])
				}
				return ""
			}
			rows = append(rows, raw{line, get("ts"), get("event"), get("subject"), get("object"), get("decision")})
		}
	}

	issue := func(line int, format string, a ...any) {
		skipped++
		if len(issues) < maxReplayIssues {
			issues = append(issues, fmt.Sprintf("line %d: ", line)+fmt.Sprintf(format, a...))
		}
	}
	for _, r := range rows {
		if strings.HasPrefix(r.event, "\x00") {
			issue(r.line, "%s, skipped", r.event[1:])
			continue
		}
		e := LogEvent{Event: strings.ToLower(r.event), Decision: strings.ToUpper(r.decision), Line: r.line}
		var ok bool
		if e.T, ok = parseTS(r.ts); !ok {
			issue(r.line, "ts %q is not an RFC 3339 timestamp or a number of seconds, skipped", r.ts)
			continue
		}
		if e.Event != "grant" && e.Event != "revoke" && e.Event != "check" {
			issue(r.line, "event %q must be grant, revoke or check, skipped", r.event)
			continue
		}
		if e.Decision != "" && e.Decision != "ALLOW" && e.Decision != "DENY" {
			issue(r.line, "decision %q must be ALLOW or DENY, skipped", r.decision)
			continue
		}
		e.Subject, e.Object = withPrefix("user:", r.subject), withPrefix("doc:", r.object)
		if err := validate(e.Subject, e.Object); err != nil {
			issue(r.line, "%v (got %q, %q), skipped", err, r.subject, r.object)
			continue
		}
		events = append(events, e)
	}
	if len(events) == 0 {
		return nil, issues, skipped, fmt.Errorf("no valid events in the log")
	}
	if len(events) > MaxReplayEvents {
		return nil, issues, skipped, fmt.Errorf("the log has %d events; the limit is %d", len(events), MaxReplayEvents)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].T.Before(events[j].T) })
	return events, issues, skipped, nil
}

func parseTS(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f < 1e12 {
		sec, frac := math.Modf(f)
		return time.Unix(int64(sec), int64(frac*1e9)).UTC(), true
	}
	return time.Time{}, false
}

// withPrefix adds the namespace to a bare ID ("alice" → "user:alice").
func withPrefix(prefix, id string) string {
	if id != "" && !strings.Contains(id, ":") {
		return prefix + id
	}
	return id
}

// ReplayStats are one strategy's results on the replayed log.
type ReplayStats struct {
	Checks      int     `json:"checks"`
	Wrong       int     `json:"wrong"`
	StaleAllows int     `json:"staleAllows"` // ALLOW after the permission was revoked
	FalseDenies int     `json:"falseDenies"` // DENY after the permission was granted
	CacheHits   int     `json:"cacheHits"`
	HitRatePct  float64 `json:"hitRatePct"`
	// SQLiteReads is how many checks read the primary store: its read load.
	SQLiteReads int     `json:"sqliteReads"`
	P50Ms       float64 `json:"p50Ms"`
	P99Ms       float64 `json:"p99Ms"`
	// MaxStaleSec is the longest time between a revocation and a stale ALLOW
	// for the same user and document: how long a revoked user kept access.
	MaxStaleSec float64 `json:"maxStaleSec"`

	lat StrategyStats
}

// ObservedStats evaluate the decisions recorded in the log itself (checks
// that carry a decision) against the replayed permission state.
type ObservedStats struct {
	Checks      int     `json:"checks"`
	Wrong       int     `json:"wrong"`
	StaleAllows int     `json:"staleAllows"`
	FalseDenies int     `json:"falseDenies"`
	MaxStaleSec float64 `json:"maxStaleSec"`
}

type ReplayResult struct {
	Events  int `json:"events"`
	Grants  int `json:"grants"`
	Revokes int `json:"revokes"`
	Checks  int `json:"checks"`
	// Skipped counts lines that were not replayed; Issues lists the first ones.
	Skipped         int                       `json:"skipped"`
	Issues          []string                  `json:"issues"`
	SpanSec         float64                   `json:"spanSec"`
	InvalidationMs  int                       `json:"invalidationMs"`
	TTLSec          int                       `json:"ttlSec"`
	ByStrategy      map[Strategy]*ReplayStats `json:"byStrategy"`
	FullyConsistent *ReplayStats              `json:"fullyConsistent"`
	Observed        *ObservedStats            `json:"observed,omitempty"`
	DurationMs      float64                   `json:"durationMs"`
}

// Replay runs an uploaded log against every strategy, plus fully consistent
// reads. Every world starts from an empty store and follows the log's
// timestamps: cache entries expire after CacheTTLSec, and each invalidation
// event is delivered invalidationMs after its commit.
func Replay(text string, invalidationMs int) (ReplayResult, error) {
	start := time.Now()
	if invalidationMs < 0 || invalidationMs > MaxInvalidationMs {
		return ReplayResult{}, fmt.Errorf("invalidation delay must be between 0 and %d ms", MaxInvalidationMs)
	}
	events, issues, skipped, err := ParseLog(text)
	if err != nil {
		return ReplayResult{}, err
	}
	res := ReplayResult{Events: len(events), Issues: issues, Skipped: skipped, InvalidationMs: invalidationMs, TTLSec: CacheTTLSec,
		SpanSec: events[len(events)-1].T.Sub(events[0].T).Seconds(), ByStrategy: map[Strategy]*ReplayStats{}, FullyConsistent: &ReplayStats{}}
	if res.Issues == nil {
		res.Issues = []string{}
	}

	now := events[0].T
	clock := func() time.Time { return now }
	type world struct {
		d       *Demo
		stats   *ReplayStats
		strong  bool
		pending []time.Time // commit times of queued invalidations, FIFO
	}
	var worlds []*world
	for _, s := range append(Strategies[:len(Strategies):len(Strategies)], RevisionAware) {
		d, err := newEmpty(s)
		if err != nil {
			return res, err
		}
		defer d.Close()
		d.Now, d.start, d.EnforceTTL = clock, now, true
		w := &world{d: d, stats: &ReplayStats{}}
		if len(worlds) == len(Strategies) {
			d.ClientTokens, w.strong, w.stats = true, true, res.FullyConsistent
		} else {
			res.ByStrategy[s] = w.stats
		}
		worlds = append(worlds, w)
	}

	viewers := map[string]bool{}
	revokedAt := map[string]time.Time{}
	var obs ObservedStats
	delay := time.Duration(invalidationMs) * time.Millisecond
	for _, e := range events {
		now = e.T
		key := e.Subject + "|" + e.Object
		for _, w := range worlds {
			for len(w.pending) > 0 && !w.pending[0].Add(delay).After(now) {
				w.d.DeliverNext()
				w.pending = w.pending[1:]
			}
		}
		switch e.Event {
		case "grant", "revoke":
			if e.Event == "revoke" && !viewers[key] {
				skipped++
				if len(res.Issues) < maxReplayIssues {
					res.Issues = append(res.Issues, fmt.Sprintf("line %d: revoke of %s on %s, which is not granted at that point, skipped", e.Line, e.Subject, e.Object))
				}
				continue
			}
			for _, w := range worlds {
				var r WriteResult
				var err error
				if e.Event == "grant" {
					r, err = w.d.Grant(e.Subject, e.Object)
				} else {
					r, err = w.d.Revoke(e.Subject, e.Object)
				}
				if err != nil {
					return res, fmt.Errorf("line %d: %w", e.Line, err)
				}
				if r.Message != nil {
					w.pending = append(w.pending, now)
				}
			}
			if e.Event == "grant" {
				res.Grants++
				viewers[key] = true
			} else {
				res.Revokes++
				delete(viewers, key)
				revokedAt[key] = now
			}
		case "check":
			res.Checks++
			stale := func() float64 { return now.Sub(revokedAt[key]).Seconds() }
			for _, w := range worlds {
				var r CheckResult
				var err error
				if w.strong {
					r, err = w.d.CheckWith(e.Subject, e.Object, CheckOpts{FullyConsistent: true})
				} else {
					r, err = w.d.Check(e.Subject, e.Object)
				}
				if err != nil {
					return res, fmt.Errorf("line %d: %w", e.Line, err)
				}
				s := w.stats
				s.Checks++
				s.lat.observe(r.LatencyMs)
				if r.ServedBy == "cache" {
					s.CacheHits++
				} else {
					s.SQLiteReads++
				}
				if !r.Correct {
					s.Wrong++
					if r.Decision == "ALLOW" {
						s.StaleAllows++
						s.MaxStaleSec = max(s.MaxStaleSec, stale())
					} else {
						s.FalseDenies++
					}
				}
			}
			if e.Decision != "" {
				obs.Checks++
				if truth := decision(viewers[key]); e.Decision != truth {
					obs.Wrong++
					if e.Decision == "ALLOW" {
						obs.StaleAllows++
						if _, ok := revokedAt[key]; ok {
							obs.MaxStaleSec = max(obs.MaxStaleSec, stale())
						}
					} else {
						obs.FalseDenies++
					}
				}
			}
		}
	}
	res.Skipped = skipped
	for _, w := range worlds {
		s := w.stats
		if s.Checks > 0 {
			s.HitRatePct = 100 * float64(s.CacheHits) / float64(s.Checks)
			s.P50Ms, s.P99Ms = s.lat.percentile(50), s.lat.percentile(99)
		}
	}
	if obs.Checks > 0 {
		res.Observed = &obs
	}
	res.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	return res, nil
}

// SampleLog is a small log for the UI: Alice is revoked and keeps reading
// for a while, Carol is denied and then granted, and Bob is revoked. The
// recorded decisions include one stale ALLOW 0.3 s after Alice's revoke.
const SampleLog = `{"ts":"2026-10-18T09:00:00Z","event":"grant","subject":"user:bob","object":"doc:7"}
{"ts":"2026-10-18T09:00:00Z","event":"grant","subject":"user:alice","object":"doc:42"}
{"ts":"2026-10-18T09:00:05Z","event":"check","subject":"user:alice","object":"doc:42","decision":"ALLOW"}
{"ts":"2026-10-18T09:00:06Z","event":"check","subject":"user:bob","object":"doc:7","decision":"ALLOW"}
{"ts":"2026-10-18T09:01:00Z","event":"revoke","subject":"user:alice","object":"doc:42"}
{"ts":"2026-10-18T09:01:00.300Z","event":"check","subject":"user:alice","object":"doc:42","decision":"ALLOW"}
{"ts":"2026-10-18T09:01:02Z","event":"check","subject":"user:alice","object":"doc:42","decision":"DENY"}
{"ts":"2026-10-18T09:02:00Z","event":"check","subject":"user:alice","object":"doc:42","decision":"DENY"}
{"ts":"2026-10-18T09:02:30Z","event":"check","subject":"user:carol","object":"doc:42","decision":"DENY"}
{"ts":"2026-10-18T09:03:00Z","event":"grant","subject":"user:carol","object":"doc:42"}
{"ts":"2026-10-18T09:03:00.200Z","event":"check","subject":"user:carol","object":"doc:42","decision":"ALLOW"}
{"ts":"2026-10-18T09:04:00Z","event":"check","subject":"user:alice","object":"doc:42","decision":"DENY"}
{"ts":"2026-10-18T09:05:59Z","event":"check","subject":"user:alice","object":"doc:42","decision":"DENY"}
{"ts":"2026-10-18T09:06:01Z","event":"check","subject":"user:alice","object":"doc:42","decision":"DENY"}
{"ts":"2026-10-18T09:06:30Z","event":"revoke","subject":"user:bob","object":"doc:7"}
{"ts":"2026-10-18T09:06:31Z","event":"check","subject":"user:bob","object":"doc:7","decision":"DENY"}
`
