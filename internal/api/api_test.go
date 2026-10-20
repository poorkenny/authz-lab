package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/poorkenny/authz-lab/internal/features"
)

func serve(t *testing.T, flags features.Flags) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewServer(flags).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Lab-Session", "test")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestControlledCacheDeliveryFlag(t *testing.T) {
	on := serve(t, features.Flags{ControlledCacheDelivery: true})
	off := serve(t, features.Flags{ControlledCacheDelivery: false})

	if _, f := call(t, off, "GET", "/api/v1/features", ""); f["controlledCacheDelivery"] != false {
		t.Errorf("features endpoint should report the flag off, got %v", f)
	}
	for _, path := range []string{"/api/v1/deliver", "/api/v1/sim/deliver", "/api/v1/sim/check"} {
		if code, _ := call(t, off, "POST", path, `{"subject":"user:alice","object":"doc:42"}`); code != http.StatusNotFound {
			t.Errorf("flag off: POST %s = %d, want 404", path, code)
		}
		if code, _ := call(t, on, "POST", path, `{"subject":"user:alice","object":"doc:42"}`); code != http.StatusOK {
			t.Errorf("flag on: POST %s = %d, want 200", path, code)
		}
	}

	// Flag off: the invalidation is delivered with the write, so the read right after is fresh.
	call(t, off, "POST", "/api/v1/strategy", `{"strategy":"invalidation"}`)
	call(t, off, "POST", "/api/v1/check", `{"subject":"user:alice","object":"doc:42"}`)
	call(t, off, "POST", "/api/v1/revoke", `{"subject":"user:alice","object":"doc:42"}`)
	if _, r := call(t, off, "POST", "/api/v1/check", `{"subject":"user:alice","object":"doc:42"}`); r["decision"] != "DENY" {
		t.Errorf("flag off: read after revoke = %v, want DENY", r["decision"])
	}

	// Flag on: the event waits in the queue, so the read right after is stale.
	call(t, on, "POST", "/api/v1/strategy", `{"strategy":"invalidation"}`)
	call(t, on, "POST", "/api/v1/check", `{"subject":"user:alice","object":"doc:42"}`)
	call(t, on, "POST", "/api/v1/revoke", `{"subject":"user:alice","object":"doc:42"}`)
	if _, r := call(t, on, "POST", "/api/v1/check", `{"subject":"user:alice","object":"doc:42"}`); r["decision"] != "ALLOW" {
		t.Errorf("flag on: read before delivery = %v, want stale ALLOW", r["decision"])
	}
}

func TestOnlyOneTestRunAtATime(t *testing.T) {
	s := NewServer(features.Defaults())
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Simulate a run in progress (from any session).
	s.testRun.Lock()
	if code, body := call(t, srv, "POST", "/api/v1/tests/run", ""); code != http.StatusConflict {
		t.Fatalf("second run while one is in progress: got %d %v, want 409", code, body)
	}
	s.testRun.Unlock()

	if code, body := call(t, srv, "POST", "/api/v1/tests/run", ""); code != http.StatusOK || body["passed"] != true {
		t.Fatalf("run after the lock is released: got %d passed=%v, want 200 and passed", code, body["passed"])
	}
}

// Regression: an empty delivery used to return "hops": null, which crashed
// the UI (blank page) when "Deliver the invalidation event" was clicked under
// a strategy without a queue. Every response must carry hops as an array.
func TestHopsAreNeverNull(t *testing.T) {
	srv := serve(t, features.Defaults())
	for _, st := range []string{"ttl", "invalidation", "revision_aware"} {
		call(t, srv, "POST", "/api/v1/strategy", `{"strategy":"`+st+`"}`)
		steps := []struct{ method, path, body string }{
			{"POST", "/api/v1/check", `{"subject":"user:alice","object":"doc:42"}`},
			{"POST", "/api/v1/revoke", `{"subject":"user:alice","object":"doc:42"}`},
			{"POST", "/api/v1/deliver", ""},
			{"POST", "/api/v1/deliver", ""}, // queue already empty
			{"POST", "/api/v1/check", `{"subject":"user:alice","object":"doc:42"}`},
		}
		for _, s := range steps {
			code, body := call(t, srv, s.method, s.path, s.body)
			if code != http.StatusOK {
				t.Fatalf("%s %s %s: status %d %v", st, s.method, s.path, code, body)
			}
			if _, ok := body["hops"].([]any); !ok {
				t.Errorf("%s %s %s: hops = %#v, want an array", st, s.method, s.path, body["hops"])
			}
		}
	}
}

func TestStrategyStoriesFlag(t *testing.T) {
	on := serve(t, features.Flags{StrategyStories: true})
	off := serve(t, features.Flags{StrategyStories: false})

	if code, _ := call(t, off, "POST", "/api/v1/clock/advance", `{"seconds":300}`); code != http.StatusNotFound {
		t.Errorf("flag off: POST /api/v1/clock/advance = %d, want 404", code)
	}

	// Flag on: the TTL story ends when the entry expires.
	alice := `{"subject":"user:alice","object":"doc:42"}`
	call(t, on, "POST", "/api/v1/strategy", `{"strategy":"ttl"}`)
	call(t, on, "POST", "/api/v1/check", alice)
	call(t, on, "POST", "/api/v1/revoke", alice)
	if _, r := call(t, on, "POST", "/api/v1/check", alice); r["decision"] != "ALLOW" || r["servedBy"] != "cache" {
		t.Errorf("read within the TTL = %v via %v, want a stale ALLOW from the cache", r["decision"], r["servedBy"])
	}
	if _, st := call(t, on, "GET", "/api/v1/state", ""); st["ttlSec"] != float64(300) || st["enforceTTL"] != true {
		t.Errorf("state should report the TTL config, got ttlSec=%v enforceTTL=%v", st["ttlSec"], st["enforceTTL"])
	}
	code, adv := call(t, on, "POST", "/api/v1/clock/advance", `{"seconds":300}`)
	if code != http.StatusOK || len(adv["evicted"].([]any)) != 1 {
		t.Fatalf("advance: %d %v, want one evicted entry", code, adv)
	}
	if _, r := call(t, on, "POST", "/api/v1/check", alice); r["decision"] != "DENY" || r["servedBy"] != "primary" {
		t.Errorf("read after the TTL = %v via %v, want DENY from SQLite", r["decision"], r["servedBy"])
	}
	if code, _ := call(t, on, "POST", "/api/v1/clock/advance", `{"seconds":0}`); code != http.StatusBadRequest {
		t.Errorf("advance by 0 s = %d, want 400", code)
	}
}

func TestHappyPathComparisonFlag(t *testing.T) {
	alice := `{"subject":"user:alice","object":"doc:42"}`
	for _, enabled := range []bool{true, false} {
		srv := serve(t, features.Flags{ControlledCacheDelivery: true, HappyPathComparison: enabled})
		call(t, srv, "POST", "/api/v1/strategy", `{"strategy":"invalidation"}`)
		call(t, srv, "POST", "/api/v1/check", alice)
		call(t, srv, "POST", "/api/v1/revoke", alice)
		_, r := call(t, srv, "POST", "/api/v1/check", alice)
		hp, ok := r["happyPath"].(map[string]any)
		if ok != enabled {
			t.Fatalf("flag %v: happyPath present = %v", enabled, ok)
		}
		if enabled && (r["decision"] != "ALLOW" || hp["decision"] != "DENY") {
			t.Errorf("read before delivery = %v with happy path %v, want ALLOW next to DENY", r["decision"], hp["decision"])
		}
	}
}

func TestClientVersionTokenFlag(t *testing.T) {
	alice := `{"subject":"user:alice","object":"doc:42"}`
	oldToken := `{"subject":"user:alice","object":"doc:42","token":3}`
	strong := `{"subject":"user:alice","object":"doc:42","consistency":"fully_consistent"}`

	off := serve(t, features.Flags{ClientVersionToken: false})
	call(t, off, "POST", "/api/v1/strategy", `{"strategy":"revision_aware"}`)
	for _, body := range []string{oldToken, strong} {
		if code, _ := call(t, off, "POST", "/api/v1/check", body); code != http.StatusNotFound {
			t.Errorf("flag off: check %s = %d, want 404", body, code)
		}
	}

	on := serve(t, features.Flags{ClientVersionToken: true})
	call(t, on, "POST", "/api/v1/strategy", `{"strategy":"revision_aware"}`)
	call(t, on, "POST", "/api/v1/check", alice)  // caches ALLOW at rev 3
	call(t, on, "POST", "/api/v1/revoke", alice) // doc:42 → ver#4
	_, r := call(t, on, "POST", "/api/v1/check", oldToken)
	if r["decision"] != "ALLOW" || r["servedBy"] != "cache" || r["contractMet"] != true || r["correct"] != false {
		t.Errorf("old token: %v via %v contractMet=%v correct=%v, want a cached ALLOW that meets the contract but is not the latest", r["decision"], r["servedBy"], r["contractMet"], r["correct"])
	}
	_, r = call(t, on, "POST", "/api/v1/check", strong)
	if r["decision"] != "DENY" || r["servedBy"] != "primary" || r["consistency"] != "fully_consistent" {
		t.Errorf("fully consistent: %v via %v (%v), want DENY from SQLite", r["decision"], r["servedBy"], r["consistency"])
	}
	if code, _ := call(t, on, "POST", "/api/v1/check", `{"subject":"user:alice","object":"doc:42","token":99}`); code != http.StatusBadRequest {
		t.Errorf("token from the future = %d, want 400", code)
	}
	call(t, on, "POST", "/api/v1/strategy", `{"strategy":"ttl"}`)
	if code, _ := call(t, on, "POST", "/api/v1/check", oldToken); code != http.StatusBadRequest {
		t.Errorf("token with the TTL strategy = %d, want 400", code)
	}
}

func TestSQLiteFailureFlag(t *testing.T) {
	off := serve(t, features.Flags{SQLiteFailure: false})
	for _, path := range []string{"/api/v1/outage/primary", "/api/v1/outage/check"} {
		if code, _ := call(t, off, "POST", path, `{"down":true}`); code != http.StatusNotFound {
			t.Errorf("flag off: POST %s = %d, want 404", path, code)
		}
	}

	// Client version tokens are always on in this simulation, whatever their own flag says.
	on := serve(t, features.Flags{SQLiteFailure: true})
	call(t, on, "POST", "/api/v1/outage/check", `{"subject":"user:alice","object":"doc:42"}`) // ALLOW cached at rev 3
	call(t, on, "POST", "/api/v1/outage/grant", `{"subject":"user:carol","object":"doc:42"}`) // doc:42 → ver#4
	if code, p := call(t, on, "POST", "/api/v1/outage/primary", `{"down":true}`); code != http.StatusOK || p["down"] != true {
		t.Fatalf("take SQLite down: %d %v", code, p)
	}
	if _, st := call(t, on, "GET", "/api/v1/outage/state", ""); st["primaryDown"] != true {
		t.Errorf("state should report SQLite down, got %v", st["primaryDown"])
	}
	code, r := call(t, on, "POST", "/api/v1/outage/check", `{"subject":"user:alice","object":"doc:42","token":3}`)
	if code != http.StatusOK || r["servedBy"] != "cache" || r["primaryDown"] != true {
		t.Errorf("token ver#3 with SQLite down: %d %v via %v, want 200 from the snapshot", code, r["decision"], r["servedBy"])
	}
	code, r = call(t, on, "POST", "/api/v1/outage/check", `{"subject":"user:alice","object":"doc:42"}`)
	ue, _ := r["unavailable"].(map[string]any)
	if code != http.StatusServiceUnavailable || !strings.Contains(r["error"].(string), "no snapshot satisfies the revision") ||
		ue["snapshotRev"] != float64(3) || ue["requiredRev"] != float64(4) {
		t.Errorf("latest token with SQLite down: %d %v, want 503 (snapshot rev 3 < ver#4)", code, r)
	}
	if code, _ := call(t, on, "POST", "/api/v1/outage/revoke", `{"subject":"user:carol","object":"doc:42"}`); code != http.StatusServiceUnavailable {
		t.Errorf("write with SQLite down = %d, want 503", code)
	}
	call(t, on, "POST", "/api/v1/outage/primary", `{"down":false}`)
	if code, r := call(t, on, "POST", "/api/v1/outage/check", `{"subject":"user:alice","object":"doc:42"}`); code != http.StatusOK || r["servedBy"] != "primary" {
		t.Errorf("after recovery: %d via %v, want 200 from SQLite", code, r["servedBy"])
	}
	// The main demo is not affected by the outage world.
	if code, r := call(t, on, "POST", "/api/v1/check", `{"subject":"user:alice","object":"doc:42"}`); code != http.StatusOK || r["primaryDown"] != false {
		t.Errorf("main demo: %d %v", code, r)
	}
}

func TestLogReplayFlag(t *testing.T) {
	off := serve(t, features.Flags{LogReplay: false})
	if code, _ := call(t, off, "GET", "/api/v1/tests/sample-log", ""); code != http.StatusNotFound {
		t.Errorf("flag off: sample log = %d, want 404", code)
	}
	if code, _ := call(t, off, "POST", "/api/v1/tests/replay", `{"log":"x"}`); code != http.StatusNotFound {
		t.Errorf("flag off: replay = %d, want 404", code)
	}

	on := serve(t, features.Flags{LogReplay: true})
	_, sample := call(t, on, "GET", "/api/v1/tests/sample-log", "")
	body, _ := json.Marshal(map[string]any{"log": sample["log"], "invalidationMs": 1000})
	code, r := call(t, on, "POST", "/api/v1/tests/replay", string(body))
	if code != http.StatusOK || r["checks"] != float64(11) {
		t.Fatalf("replay of the sample log: %d %v", code, r)
	}
	if ra := r["byStrategy"].(map[string]any)["revision_aware"].(map[string]any); ra["wrong"] != float64(0) {
		t.Errorf("revision-aware on the sample log: %v", ra)
	}
	if code, _ := call(t, on, "POST", "/api/v1/tests/replay", `{"log":"not a log"}`); code != http.StatusBadRequest {
		t.Errorf("invalid log = %d, want 400", code)
	}
	// Logs larger than the normal 16 KB request limit are accepted.
	big := strings.Repeat(`{"ts":1,"event":"grant","subject":"alice","object":"42"}`+"\n", 400)
	body, _ = json.Marshal(map[string]any{"log": big})
	if code, r := call(t, on, "POST", "/api/v1/tests/replay", string(body)); code != http.StatusOK || r["grants"] != float64(400) {
		t.Errorf("%d-byte log: %d %v", len(body), code, r["error"])
	}
}
