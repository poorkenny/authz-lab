// Package api exposes the strategy showcase over HTTP. Every request is
// scoped to a reviewer session via the X-Lab-Session header (the UI generates
// one per browser), so reviewers on a shared instance never see each other's
// state. A session holds up to three worlds: the main demo (any strategy),
// the invalidation-queue simulation (always the invalidation strategy) and
// the SQLite-failure simulation (revision-aware, created on first use).
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/poorkenny/authz-lab/internal/audit"
	"github.com/poorkenny/authz-lab/internal/demo"
	"github.com/poorkenny/authz-lab/internal/features"
)

const (
	maxBody     = 16 << 10
	maxLogBody  = 2 << 20 // an uploaded log for the benchmark replay
	maxSessions = 500
	sessionTTL  = 30 * time.Minute
	ratePerSec  = 10
	rateBurst   = 40
	sessionHdr  = "X-Lab-Session"
)

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type session struct {
	mu       sync.Mutex
	demo     *demo.Demo
	sim      *demo.Demo
	outage   *demo.Demo // nil until the SQLite-failure simulation is used
	lastUsed time.Time
	tokens   float64
	tokensAt time.Time
}

func (s *session) allow() bool {
	now := time.Now()
	s.tokens = min(rateBurst, s.tokens+now.Sub(s.tokensAt).Seconds()*ratePerSec)
	s.tokensAt = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

type Server struct {
	mu       sync.Mutex
	sessions map[string]*session
	flags    features.Flags
	// testRun allows one correctness-suite run at a time across all sessions,
	// so concurrent reviewers can't exhaust a small (free-tier) instance.
	testRun sync.Mutex
}

func NewServer(flags features.Flags) *Server {
	s := &Server{sessions: map[string]*session{}, flags: flags}
	go func() {
		for range time.Tick(time.Minute) {
			s.mu.Lock()
			for id, ss := range s.sessions {
				if time.Since(ss.lastUsed) > sessionTTL {
					s.drop(id)
				}
			}
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *Server) session(id string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss, ok := s.sessions[id]; ok {
		ss.lastUsed = time.Now()
		return ss, nil
	}
	if len(s.sessions) >= maxSessions {
		var oldestID string
		var oldest time.Time
		for k, v := range s.sessions {
			if oldestID == "" || v.lastUsed.Before(oldest) {
				oldestID, oldest = k, v.lastUsed
			}
		}
		s.drop(oldestID)
	}
	d, err := s.newDemo(demo.TTL)
	if err != nil {
		return nil, err
	}
	sim, err := demo.New(demo.Invalidation)
	if err != nil {
		d.Close()
		return nil, err
	}
	ss := &session{demo: d, sim: sim, lastUsed: time.Now(), tokens: rateBurst, tokensAt: time.Now()}
	s.sessions[id] = ss
	return ss, nil
}

// drop removes a session and closes its SQLite databases. Callers hold s.mu.
func (s *Server) drop(id string) {
	ss := s.sessions[id]
	delete(s.sessions, id)
	go func() {
		ss.mu.Lock()
		defer ss.mu.Unlock()
		ss.demo.Close()
		ss.sim.Close()
		if ss.outage != nil {
			ss.outage.Close()
		}
	}()
}

// newDemo creates a main-demo world configured by the feature flags.
func (s *Server) newDemo(st demo.Strategy) (*demo.Demo, error) {
	d, err := demo.New(st)
	if err != nil {
		return nil, err
	}
	d.Options = s.options()
	return d, nil
}

// options maps the feature flags onto demo behaviours.
func (s *Server) options() demo.Options {
	return demo.Options{
		AutoDeliver:  !s.flags.ControlledCacheDelivery,
		EnforceTTL:   s.flags.StrategyStories,
		HappyPath:    s.flags.HappyPathComparison,
		ClientTokens: s.flags.ClientVersionToken,
	}
}

// requireStrategyStories rejects requests for the strategy-stories feature
// (the simulated TTL clock) when its flag is off.
func (s *Server) requireStrategyStories() error {
	if !s.flags.StrategyStories {
		return httpError{http.StatusNotFound, "strategy stories are disabled (FEATURE_STRATEGY_STORIES=false)"}
	}
	return nil
}

// newOutage creates the SQLite-failure world: revision-aware, with client
// version tokens so a request can ask for an older revision.
func newOutage() (*demo.Demo, error) {
	d, err := demo.New(demo.RevisionAware)
	if err != nil {
		return nil, err
	}
	d.ClientTokens = true
	return d, nil
}

// requireSQLiteFailure rejects requests for the SQLite-failure simulation
// when its flag is off.
func (s *Server) requireSQLiteFailure() error {
	if !s.flags.SQLiteFailure {
		return httpError{http.StatusNotFound, "the SQLite failure simulation is disabled (FEATURE_SQLITE_FAILURE=false)"}
	}
	return nil
}

// requireLogReplay rejects benchmark log uploads when their flag is off.
func (s *Server) requireLogReplay() error {
	if !s.flags.LogReplay {
		return httpError{http.StatusNotFound, "log replay is disabled (FEATURE_LOG_REPLAY=false)"}
	}
	return nil
}

// requireControlledDelivery rejects requests for the controlled-cache-delivery
// feature when its flag is off.
func (s *Server) requireControlledDelivery() error {
	if !s.flags.ControlledCacheDelivery {
		return httpError{http.StatusNotFound, "controlled cache delivery is disabled (FEATURE_CONTROLLED_CACHE_DELIVERY=false)"}
	}
	return nil
}

type world int

const (
	mainWorld world = iota
	simWorld
	outageWorld
)

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.sessions)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessions": n})
	})
	mux.HandleFunc("GET /api/v1/features", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.flags)
	})

	// Routes shared by both worlds: /api/v1/... (main) and /api/v1/sim/... (queue simulation).
	for prefix, wd := range map[string]world{"/api/v1": mainWorld, "/api/v1/sim": simWorld} {
		mux.HandleFunc("GET "+prefix+"/state", s.with(wd, func(r *http.Request, d **demo.Demo) (any, error) {
			return (*d).State()
		}))
		mux.HandleFunc("POST "+prefix+"/reset", s.with(wd, func(r *http.Request, d **demo.Demo) (any, error) {
			if wd == simWorld {
				return s.reset(d, demo.Invalidation, false)
			}
			return s.reset(d, (*d).Strategy, true)
		}))
		mux.HandleFunc("POST "+prefix+"/grant", s.with(wd, func(r *http.Request, d **demo.Demo) (any, error) {
			return withPair(r, (*d).Grant)
		}))
		mux.HandleFunc("POST "+prefix+"/revoke", s.with(wd, func(r *http.Request, d **demo.Demo) (any, error) {
			return withPair(r, (*d).Revoke)
		}))
		mux.HandleFunc("POST "+prefix+"/check", s.with(wd, func(r *http.Request, d **demo.Demo) (any, error) {
			if wd == simWorld {
				return withPair(r, (*d).Check)
			}
			return s.check(r, *d, true)
		}))
		mux.HandleFunc("POST "+prefix+"/deliver", s.with(wd, func(r *http.Request, d **demo.Demo) (any, error) {
			if err := s.requireControlledDelivery(); err != nil {
				return nil, err
			}
			return (*d).DeliverNext(), nil
		}))
	}

	// The SQLite-failure simulation (FEATURE_SQLITE_FAILURE).
	mux.HandleFunc("GET /api/v1/outage/state", s.with(outageWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		return (*d).State()
	}))
	mux.HandleFunc("POST /api/v1/outage/reset", s.with(outageWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		fresh, err := newOutage()
		if err != nil {
			return nil, err
		}
		(*d).Close()
		*d = fresh
		return fresh.State()
	}))
	mux.HandleFunc("POST /api/v1/outage/grant", s.with(outageWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		return withPair(r, (*d).Grant)
	}))
	mux.HandleFunc("POST /api/v1/outage/revoke", s.with(outageWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		return withPair(r, (*d).Revoke)
	}))
	mux.HandleFunc("POST /api/v1/outage/check", s.with(outageWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		return s.check(r, *d, false)
	}))
	mux.HandleFunc("POST /api/v1/outage/primary", s.with(outageWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		var req struct {
			Down bool `json:"down"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return (*d).SetPrimaryDown(req.Down), nil
	}))

	mux.HandleFunc("POST /api/v1/strategy", s.with(mainWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		var req struct {
			Strategy string `json:"strategy"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		st, err := demo.ParseStrategy(req.Strategy)
		if err != nil {
			return nil, badRequest(err)
		}
		// A strategy change starts a fresh world so runs are comparable.
		return s.reset(d, st, true)
	}))
	mux.HandleFunc("POST /api/v1/clock/advance", s.with(mainWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		if err := s.requireStrategyStories(); err != nil {
			return nil, err
		}
		var req struct {
			Seconds int `json:"seconds"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		if req.Seconds < 1 || req.Seconds > 3600 {
			return nil, badRequest(errors.New("seconds must be between 1 and 3600"))
		}
		return (*d).Advance(req.Seconds), nil
	}))
	mux.HandleFunc("POST /api/v1/sim/other-writes", s.with(simWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		var req struct {
			Count int `json:"count"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		if req.Count < 1 || req.Count > 10 {
			return nil, badRequest(errors.New("count must be between 1 and 10"))
		}
		if (*d).Queue.Depth()+req.Count > 30 {
			return nil, badRequest(errors.New("the simulation queue holds at most 30 events; deliver some first"))
		}
		return (*d).AddOtherWrites(req.Count)
	}))
	mux.HandleFunc("GET /api/v1/compare", s.with(mainWorld, func(r *http.Request, _ **demo.Demo) (any, error) {
		type row struct {
			Strategy demo.Strategy    `json:"strategy"`
			Before   demo.CheckResult `json:"before"`
			After    demo.CheckResult `json:"after"`
		}
		var rows []row
		for _, st := range demo.Strategies {
			before, after, err := demo.Story(st, s.options())
			if err != nil {
				return nil, err
			}
			rows = append(rows, row{st, before, after})
		}
		return map[string]any{"rows": rows}, nil
	}))
	mux.HandleFunc("GET /api/v1/audit", s.with(mainWorld, func(r *http.Request, d **demo.Demo) (any, error) {
		return map[string]any{"records": (*d).Audit.Query(audit.Filter{Limit: 200, StaleOnly: r.URL.Query().Get("stale") == "true"})}, nil
	}))
	mux.HandleFunc("POST /api/v1/tests/run", s.with(mainWorld, func(r *http.Request, _ **demo.Demo) (any, error) {
		// Reject instead of waiting, so requests can't pile up behind a run.
		if !s.testRun.TryLock() {
			return nil, httpError{http.StatusConflict, "Another benchmark run is in progress on this server. Try again in a few seconds."}
		}
		defer s.testRun.Unlock()
		return demo.RunSuite()
	}))
	mux.HandleFunc("GET /api/v1/tests/sample-log", s.with(mainWorld, func(r *http.Request, _ **demo.Demo) (any, error) {
		if err := s.requireLogReplay(); err != nil {
			return nil, err
		}
		return map[string]any{"log": demo.SampleLog, "invalidationMs": demo.DefaultInvalidationMs}, nil
	}))
	mux.HandleFunc("POST /api/v1/tests/replay", s.withBody(mainWorld, maxLogBody, func(r *http.Request, _ **demo.Demo) (any, error) {
		if err := s.requireLogReplay(); err != nil {
			return nil, err
		}
		req := struct {
			Log            string `json:"log"`
			InvalidationMs *int   `json:"invalidationMs"`
		}{}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		ms := demo.DefaultInvalidationMs
		if req.InvalidationMs != nil {
			ms = *req.InvalidationMs
		}
		// Shares the suite's lock: one benchmark run at a time per server.
		if !s.testRun.TryLock() {
			return nil, httpError{http.StatusConflict, "Another benchmark run is in progress on this server. Try again in a few seconds."}
		}
		defer s.testRun.Unlock()
		out, err := demo.Replay(req.Log, ms)
		if err != nil {
			return nil, badRequest(err)
		}
		return out, nil
	}))
}

type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func badRequest(err error) error { return httpError{http.StatusBadRequest, err.Error()} }

// with resolves the session, rate-limits it and serializes access to the chosen world.
func (s *Server) with(wd world, h func(r *http.Request, d **demo.Demo) (any, error)) http.HandlerFunc {
	return s.withBody(wd, maxBody, h)
}

// withBody is with, accepting request bodies of up to limit bytes.
func (s *Server) withBody(wd world, limit int64, h func(r *http.Request, d **demo.Demo) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(sessionHdr)
		if id == "" {
			id = "anonymous"
		}
		if !sessionRe.MatchString(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid " + sessionHdr})
			return
		}
		ss, err := s.session(id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		ss.mu.Lock()
		defer ss.mu.Unlock()
		if !ss.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded, slow down"})
			return
		}
		target := &ss.demo
		switch wd {
		case simWorld:
			if err := s.requireControlledDelivery(); err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
				return
			}
			target = &ss.sim
		case outageWorld:
			if err := s.requireSQLiteFailure(); err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
				return
			}
			if ss.outage == nil {
				if ss.outage, err = newOutage(); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
			}
			target = &ss.outage
		}
		out, err := h(r, target)
		if err != nil {
			// SQLite is down and no cached snapshot can answer: 503, with the
			// details (requested revision, snapshot revision, hops) for the UI.
			var ue *demo.UnavailableError
			if errors.As(err, &ue) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": ue.Message, "unavailable": ue})
				return
			}
			status := http.StatusInternalServerError
			var he httpError
			if errors.As(err, &he) {
				status = he.status
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// reset replaces a world with a fresh one; main-demo worlds follow the feature flags.
func (s *Server) reset(d **demo.Demo, st demo.Strategy, main bool) (any, error) {
	var fresh *demo.Demo
	var err error
	if main {
		fresh, err = s.newDemo(st)
	} else {
		fresh, err = demo.New(st)
	}
	if err != nil {
		return nil, err
	}
	(*d).Close()
	*d = fresh
	return fresh.State()
}

// check handles a check that may carry revision-aware options: the client's
// version token, or "consistency": "fully_consistent". With gated, the
// options need FEATURE_CLIENT_VERSION_TOKEN (main demo); the SQLite-failure
// simulation always accepts them.
func (s *Server) check(r *http.Request, d *demo.Demo, gated bool) (any, error) {
	var req struct {
		Subject     string `json:"subject"`
		Object      string `json:"object"`
		Token       int64  `json:"token"`
		Consistency string `json:"consistency"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	opts := demo.CheckOpts{Token: req.Token}
	switch req.Consistency {
	case "", "at_least_as_fresh":
	case "fully_consistent":
		opts.FullyConsistent = true
	default:
		return nil, badRequest(errors.New(`consistency must be "at_least_as_fresh" or "fully_consistent"`))
	}
	if gated && opts != (demo.CheckOpts{}) && !s.flags.ClientVersionToken {
		return nil, httpError{http.StatusNotFound, "client version tokens are disabled (FEATURE_CLIENT_VERSION_TOKEN=false)"}
	}
	out, err := d.CheckWith(req.Subject, req.Object, opts)
	if err != nil {
		return nil, passUnavailable(err)
	}
	return out, nil
}

// passUnavailable keeps a 503 as it is and turns any other error into a 400.
func passUnavailable(err error) error {
	var ue *demo.UnavailableError
	if errors.As(err, &ue) {
		return err
	}
	return badRequest(err)
}

func withPair[T any](r *http.Request, f func(subject, object string) (T, error)) (any, error) {
	var req struct {
		Subject string `json:"subject"`
		Object  string `json:"object"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	out, err := f(req.Subject, req.Object)
	if err != nil {
		return nil, passUnavailable(err)
	}
	return out, nil
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest(err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
