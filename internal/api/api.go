// Package api exposes the strategy showcase over HTTP. Every request is scoped
// to a reviewer session via the X-Lab-Session header (the UI generates one per
// browser), so reviewers on a shared instance never see each other's state.
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
}

func NewServer() *Server {
	s := &Server{sessions: map[string]*session{}}
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
	d, err := demo.New(demo.TTL)
	if err != nil {
		return nil, err
	}
	ss := &session{demo: d, lastUsed: time.Now(), tokens: rateBurst, tokensAt: time.Now()}
	s.sessions[id] = ss
	return ss, nil
}

// drop removes a session and closes its SQLite database. Callers hold s.mu.
func (s *Server) drop(id string) {
	ss := s.sessions[id]
	delete(s.sessions, id)
	go func() {
		ss.mu.Lock()
		defer ss.mu.Unlock()
		ss.demo.Close()
	}()
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.sessions)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessions": n})
	})
	mux.HandleFunc("GET /api/v1/state", s.with(func(r *http.Request, d **demo.Demo) (any, error) {
		return (*d).State()
	}))
	mux.HandleFunc("POST /api/v1/reset", s.with(func(r *http.Request, d **demo.Demo) (any, error) {
		return reset(d, (*d).Strategy)
	}))
	mux.HandleFunc("POST /api/v1/strategy", s.with(func(r *http.Request, d **demo.Demo) (any, error) {
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
		return reset(d, st)
	}))
	mux.HandleFunc("POST /api/v1/grant", s.with(func(r *http.Request, d **demo.Demo) (any, error) {
		return withPair(r, *d, (*d).Grant)
	}))
	mux.HandleFunc("POST /api/v1/revoke", s.with(func(r *http.Request, d **demo.Demo) (any, error) {
		return withPair(r, *d, (*d).Revoke)
	}))
	mux.HandleFunc("POST /api/v1/check", s.with(func(r *http.Request, d **demo.Demo) (any, error) {
		return withPair(r, *d, (*d).Check)
	}))
	mux.HandleFunc("GET /api/v1/audit", s.with(func(r *http.Request, d **demo.Demo) (any, error) {
		return map[string]any{"records": (*d).Audit.Query(audit.Filter{Limit: 200, StaleOnly: r.URL.Query().Get("stale") == "true"})}, nil
	}))
	mux.HandleFunc("GET /api/v1/compare", s.with(func(r *http.Request, _ **demo.Demo) (any, error) {
		type row struct {
			Strategy demo.Strategy    `json:"strategy"`
			Soon     demo.CheckResult `json:"soon"`
			Later    demo.CheckResult `json:"later"`
		}
		var rows []row
		for _, st := range demo.Strategies {
			soon, later, err := demo.Story(st)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row{st, soon, later})
		}
		return map[string]any{"rows": rows}, nil
	}))
	mux.HandleFunc("POST /api/v1/tests/run", s.with(func(r *http.Request, _ **demo.Demo) (any, error) {
		return demo.RunSuite()
	}))
}

type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func badRequest(err error) error { return httpError{http.StatusBadRequest, err.Error()} }

// with resolves the session, rate-limits it and serializes access to its demo.
func (s *Server) with(h func(r *http.Request, d **demo.Demo) (any, error)) http.HandlerFunc {
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
		out, err := h(r, &ss.demo)
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

func reset(d **demo.Demo, st demo.Strategy) (any, error) {
	fresh, err := demo.New(st)
	if err != nil {
		return nil, err
	}
	(*d).Close()
	*d = fresh
	return fresh.State()
}

// withPair decodes {subject, object, waitMs}, advances the simulated clock by
// waitMs, then runs f.
func withPair[T any](r *http.Request, d *demo.Demo, f func(subject, object string) (T, error)) (any, error) {
	var req struct {
		Subject string `json:"subject"`
		Object  string `json:"object"`
		WaitMs  int64  `json:"waitMs"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.WaitMs < 0 || req.WaitMs > 3_600_000 {
		return nil, badRequest(errors.New("waitMs must be between 0 and 3600000"))
	}
	d.Advance(req.WaitMs)
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
