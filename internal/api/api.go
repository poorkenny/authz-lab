// Package api exposes the mock authorization system over HTTP:
// data store (tuples), check engine, audit log, and lab control.
//
// Every request is scoped to a reviewer session via the X-Lab-Session header
// (the UI generates one per browser).
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/poorkenny/authz-lab/internal/audit"
	"github.com/poorkenny/authz-lab/internal/engine"
	"github.com/poorkenny/authz-lab/internal/lab"
	"github.com/poorkenny/authz-lab/internal/sim"
	"github.com/poorkenny/authz-lab/internal/store"
)

const (
	maxBody      = 64 << 10
	ratePerSec   = 10
	rateBurst    = 40
	sessionHdr   = "X-Lab-Session"
	defaultSessn = "anonymous"
)

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Server struct {
	Sessions *lab.Manager
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessions": s.Sessions.Count()})
	})
	mux.HandleFunc("GET /api/v1/schema", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"schema": engine.Schema})
	})

	// Data store layer
	mux.HandleFunc("GET /api/v1/tuples", s.with(s.listTuples))
	mux.HandleFunc("POST /api/v1/tuples/write", s.with(s.writeTuples))
	mux.HandleFunc("GET /api/v1/changelog", s.with(s.changelog))

	// Check engine
	mux.HandleFunc("POST /api/v1/check", s.with(s.check))

	// Audit log
	mux.HandleFunc("GET /api/v1/audit", s.with(s.queryAudit))
	mux.HandleFunc("GET /api/v1/audit/export", s.with(s.exportAudit))
	mux.HandleFunc("GET /api/v1/audit/{id}", s.with(s.getAudit))

	// Lab control
	mux.HandleFunc("GET /api/v1/lab/state", s.with(s.state))
	mux.HandleFunc("GET /api/v1/lab/timeline", s.with(s.timeline))
	mux.HandleFunc("GET /api/v1/lab/config", s.with(s.getConfig))
	mux.HandleFunc("PUT /api/v1/lab/config", s.with(s.putConfig))
	mux.HandleFunc("POST /api/v1/lab/reset", s.with(s.reset))
	mux.HandleFunc("POST /api/v1/lab/advance", s.with(s.advance))
	mux.HandleFunc("POST /api/v1/lab/partition", s.with(s.partition))
	mux.HandleFunc("POST /api/v1/lab/drill", s.with(s.drill))
	mux.HandleFunc("POST /api/v1/lab/experiments/lag-sweep", s.with(s.lagSweep))
}

type handler func(w http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error)

type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func badRequest(err error) error { return httpError{http.StatusBadRequest, err.Error()} }

// with resolves the session, applies rate limiting and serializes access.
func (s *Server) with(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(sessionHdr)
		if id == "" {
			id = defaultSessn
		}
		if !sessionRe.MatchString(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid " + sessionHdr})
			return
		}
		sess, err := s.Sessions.Get(id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)

		sess.Lock()
		defer sess.Unlock()
		if !sess.Allow(ratePerSec, rateBurst) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded, slow down"})
			return
		}
		out, err := h(w, r, sess)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest(err)
	}
	return nil
}

func (s *Server) listTuples(_ http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error) {
	w := sess.World
	rev := w.Store.Rev()
	if v := r.URL.Query().Get("atRev"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, badRequest(errors.New("atRev must be a non-negative integer"))
		}
		rev = min(n, rev)
	}
	return map[string]any{"rev": rev, "tuples": w.Store.Tuples(rev, r.URL.Query().Get("object"))}, nil
}

type writeReq struct {
	Writes []struct {
		Op    store.Op `json:"op"`
		Tuple string   `json:"tuple"`
	} `json:"writes"`
}

func (s *Server) writeTuples(_ http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error) {
	var req writeReq
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if len(req.Writes) == 0 || len(req.Writes) > 20 {
		return nil, badRequest(errors.New("send 1-20 writes"))
	}
	var changes []store.Change
	var tok string
	for _, wr := range req.Writes {
		t, err := store.ParseTuple(wr.Tuple)
		if err != nil {
			return nil, badRequest(err)
		}
		c, tk, err := sess.World.Write(wr.Op, t)
		if err != nil {
			return nil, badRequest(err)
		}
		changes, tok = append(changes, c), tk
	}
	return map[string]any{"revision": sess.World.Store.Rev(), "token": tok, "changes": changes}, nil
}

func (s *Server) changelog(_ http.ResponseWriter, _ *http.Request, sess *lab.Session) (any, error) {
	return map[string]any{"changes": sess.World.Store.Changes()}, nil
}

func (s *Server) check(_ http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error) {
	var req sim.CheckRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	res, err := sess.World.Check(req)
	if err != nil {
		return nil, badRequest(err)
	}
	return res, nil
}

func (s *Server) queryAudit(_ http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	recs := sess.Audit.Query(audit.Filter{
		Kind: q.Get("kind"), RunID: q.Get("runId"), Subject: q.Get("subject"), Object: q.Get("object"),
		StaleOnly: q.Get("stale") == "true", Limit: limit,
	})
	return map[string]any{"records": recs}, nil
}

func (s *Server) exportAudit(w http.ResponseWriter, _ *http.Request, sess *lab.Session) (any, error) {
	w.Header().Set("Content-Disposition", `attachment; filename="authz-lab-audit.json"`)
	return map[string]any{"records": sess.Audit.All()}, nil
}

func (s *Server) getAudit(_ http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error) {
	rec, ok := sess.Audit.Get(r.PathValue("id"))
	if !ok {
		return nil, httpError{http.StatusNotFound, "audit record not found"}
	}
	return rec, nil
}

func (s *Server) state(_ http.ResponseWriter, _ *http.Request, sess *lab.Session) (any, error) {
	return sess.World.State(), nil
}

func (s *Server) timeline(_ http.ResponseWriter, _ *http.Request, sess *lab.Session) (any, error) {
	return map[string]any{"events": sess.World.Timeline()}, nil
}

func (s *Server) getConfig(_ http.ResponseWriter, _ *http.Request, sess *lab.Session) (any, error) {
	return sess.World.Config(), nil
}

func (s *Server) putConfig(_ http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error) {
	var cfg sim.Config
	if err := decode(r, &cfg); err != nil {
		return nil, err
	}
	sess.World.SetConfig(cfg)
	return sess.World.Config(), nil
}

func (s *Server) reset(_ http.ResponseWriter, _ *http.Request, sess *lab.Session) (any, error) {
	if err := sess.Reset(sess.World.Config()); err != nil {
		return nil, err
	}
	return sess.World.State(), nil
}

func (s *Server) advance(_ http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error) {
	var req struct {
		Ms int64 `json:"ms"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.Ms <= 0 || req.Ms > 600_000 {
		return nil, badRequest(errors.New("ms must be in (0, 600000]"))
	}
	sess.World.Advance(req.Ms)
	return sess.World.State(), nil
}

func (s *Server) partition(_ http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error) {
	var req struct {
		Replica string `json:"replica"`
		Heal    bool   `json:"heal"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	var err error
	if req.Heal {
		err = sess.World.Heal(req.Replica)
	} else {
		err = sess.World.Partition(req.Replica)
	}
	if err != nil {
		return nil, badRequest(err)
	}
	return sess.World.State(), nil
}

func (s *Server) drill(_ http.ResponseWriter, r *http.Request, sess *lab.Session) (any, error) {
	var cfg sim.Config
	if err := decode(r, &cfg); err != nil {
		return nil, err
	}
	res, w, err := lab.RunDrill(cfg, sess.NextRunID(), sess.Audit)
	if err != nil {
		return nil, err
	}
	sess.World = w
	return res, nil
}

func (s *Server) lagSweep(_ http.ResponseWriter, r *http.Request, _ *lab.Session) (any, error) {
	var req lab.SweepRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	res, err := lab.RunLagSweep(req)
	if err != nil {
		return nil, badRequest(err)
	}
	return res, nil
}
