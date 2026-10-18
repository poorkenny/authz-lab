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
