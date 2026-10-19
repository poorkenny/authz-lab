package demo

import (
	"strings"
	"testing"
)

func TestReplaySampleLog(t *testing.T) {
	res, err := Replay(SampleLog, DefaultInvalidationMs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 0 || res.Grants != 3 || res.Revokes != 2 || res.Checks != 11 {
		t.Fatalf("counts: %+v", res)
	}
	ttl, inv, ra, fc := res.ByStrategy[TTL], res.ByStrategy[Invalidation], res.ByStrategy[RevisionAware], res.FullyConsistent
	// TTL: Alice's ALLOW cached at 09:00:05 is served until it expires at 09:05:05.
	if ttl.StaleAllows == 0 || ttl.MaxStaleSec != 180 || ttl.FalseDenies == 0 {
		t.Errorf("ttl: %+v", ttl)
	}
	// Invalidation: stale only within the 1 s delivery delay.
	if inv.StaleAllows != 1 || inv.MaxStaleSec > 1 || inv.FalseDenies != 1 {
		t.Errorf("invalidation: %+v", inv)
	}
	if ra.Wrong != 0 || fc.Wrong != 0 || fc.CacheHits != 0 || fc.SQLiteReads != res.Checks || ra.SQLiteReads >= fc.SQLiteReads {
		t.Errorf("revision-aware %+v, fully consistent %+v", ra, fc)
	}
	if o := res.Observed; o == nil || o.Checks != 11 || o.StaleAllows != 1 || o.Wrong != 1 {
		t.Errorf("observed: %+v", res.Observed)
	}
	// With instant delivery, invalidation is never stale on this log.
	res0, err := Replay(SampleLog, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res0.ByStrategy[Invalidation].Wrong != 0 {
		t.Errorf("invalidation with 0 ms delay: %+v", res0.ByStrategy[Invalidation])
	}
}

func TestReplayCSVAndIssues(t *testing.T) {
	log := `ts,event,subject,object
0,grant,alice,42
10,check,alice,42
20,revoke,alice,42
20.5,check,alice,42
21,revoke,alice,42
nope,check,alice,42
30,delete,alice,42
40,check,group:x,42
`
	res, err := Replay(log, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Checks != 2 || res.Skipped != 4 || len(res.Issues) != 4 {
		t.Fatalf("checks %d skipped %d issues %v", res.Checks, res.Skipped, res.Issues)
	}
	if res.SpanSec != 21 || res.ByStrategy[TTL].StaleAllows != 1 || res.ByStrategy[TTL].MaxStaleSec != 0.5 {
		t.Errorf("span %v, ttl %+v", res.SpanSec, res.ByStrategy[TTL])
	}
	for _, bad := range []string{"", "ts,event\n1,check", "{\"ts\":\"x\"}"} {
		if _, err := Replay(bad, 1000); err == nil {
			t.Errorf("Replay(%q) should fail", bad)
		}
	}
	if _, err := Replay(log, -1); err == nil || !strings.Contains(err.Error(), "invalidation delay") {
		t.Errorf("negative delay: %v", err)
	}
}
