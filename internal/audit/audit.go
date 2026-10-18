// Package audit records every write and check decision together with an
// oracle verdict (the same check evaluated at the primary's latest revision),
// so a stale decision is visible in the log itself.
package audit

import (
	"strings"
)

const maxRecords = 5000

type Record struct {
	ID             string   `json:"id"`
	RunID          string   `json:"runId,omitempty"`
	T              int64    `json:"t"`    // simulated ms
	Kind           string   `json:"kind"` // write | check
	Subject        string   `json:"subject,omitempty"`
	Permission     string   `json:"permission,omitempty"`
	Object         string   `json:"object,omitempty"`
	Tuple          string   `json:"tuple,omitempty"`
	Op             string   `json:"op,omitempty"`
	Rev            int64    `json:"rev,omitempty"`
	Decision       string   `json:"decision,omitempty"`
	OracleDecision string   `json:"oracleDecision,omitempty"`
	Stale          bool     `json:"stale"`
	FalseDeny      bool     `json:"falseDeny"`
	ServedBy       string   `json:"servedBy,omitempty"`
	SnapshotRev    int64    `json:"snapshotRev,omitempty"`
	RequiredRev    int64    `json:"requiredRev,omitempty"`
	WatermarkRev   int64    `json:"watermarkRev,omitempty"`
	PrimaryRev     int64    `json:"primaryRev,omitempty"`
	Path           string   `json:"path,omitempty"`
	LatencyMs      float64  `json:"latencyMs,omitempty"`
	DefenseApplied string   `json:"defenseApplied,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

// Log is an in-memory, bounded audit log. Not safe for concurrent use; the
// owning session serializes access.
type Log struct {
	records []Record
}

func (l *Log) Append(r Record) {
	l.records = append(l.records, r)
	if len(l.records) > maxRecords {
		l.records = l.records[len(l.records)-maxRecords:]
	}
}

func (l *Log) Get(id string) (Record, bool) {
	for i := len(l.records) - 1; i >= 0; i-- {
		if l.records[i].ID == id {
			return l.records[i], true
		}
	}
	return Record{}, false
}

type Filter struct {
	Kind      string
	RunID     string
	Subject   string
	Object    string
	StaleOnly bool
	Limit     int
}

// Query returns matching records, newest first.
func (l *Log) Query(f Filter) []Record {
	if f.Limit <= 0 || f.Limit > maxRecords {
		f.Limit = 200
	}
	out := []Record{}
	for i := len(l.records) - 1; i >= 0 && len(out) < f.Limit; i-- {
		r := l.records[i]
		if f.Kind != "" && r.Kind != f.Kind ||
			f.RunID != "" && r.RunID != f.RunID ||
			f.Subject != "" && r.Subject != f.Subject ||
			f.Object != "" && r.Object != f.Object && !strings.HasPrefix(r.Tuple, f.Object+"#") ||
			f.StaleOnly && !r.Stale {
			continue
		}
		out = append(out, r)
	}
	return out
}

// All returns every record, oldest first.
func (l *Log) All() []Record { return append([]Record(nil), l.records...) }
