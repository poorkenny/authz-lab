// Package api exposes the strategy showcase over HTTP. Every request is
// scoped to a reviewer session via the X-Lab-Session header (the UI generates
// one per browser), so reviewers on a shared instance never see each other's
// state. A session holds two worlds: the main demo (any strategy) and the
// invalidation-queue simulation (always the invalidation strategy).
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
	}()
}

// newDemo creates a main-demo world configured by the feature flags.
func (s *Server) newDemo(st demo.Strategy) (*demo.Demo, error) {
	d, err := demo.New(st)
	if err != nil {
		return nil, err
	}
	d.AutoDeliver = !s.flags.ControlledCacheDelivery
	return d, nil
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
			return withPair(r, (*d).Check)
		}))
		mux.HandleFunc("POST "+prefix+"/deliver", s.with(wd, func(r *http.Request, d **demo.Demo) (any, error) {
			if err := s.requireControlledDelivery(); err != nil {
				return nil, err
			}
			return (*d).DeliverNext(), nil
		}))
	}

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
			before, after, err := demo.Story(st, !s.flags.ControlledCacheDelivery)
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
			return nil, httpError{http.StatusConflict, "Another correctness test run is in progress on this server. Try again in a few seconds."}
		}
		defer s.testRun.Unlock()
		return demo.RunSuite()
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
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		ss.mu.Lock()
		defer ss.mu.Unlock()
		if !ss.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded, slow down"})
			return
		}
		target := &ss.demo
		if wd == simWorld {
			if err := s.requireControlledDelivery(); err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
				return
			}
			target = &ss.sim
		}
		out, err := h(r, target)
		if err != nil {
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
		return nil, badRequest(err)
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
