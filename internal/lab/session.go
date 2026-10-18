package lab

import (
	"fmt"
	"sync"
	"time"

	"github.com/poorkenny/authz-lab/internal/audit"
	"github.com/poorkenny/authz-lab/internal/sim"
	"github.com/poorkenny/authz-lab/internal/store"
)

// Session isolates one reviewer: their own world, config and audit log, so
// reviewers on a shared public instance never see each other's runs.
type Session struct {
	mu       sync.Mutex
	ID       string
	World    *sim.World
	Audit    *audit.Log
	lastUsed time.Time
	runs     int

	tokens   float64
	tokensAt time.Time
}

// Lock serializes all access to a session.
func (s *Session) Lock()   { s.mu.Lock() }
func (s *Session) Unlock() { s.mu.Unlock() }

func (s *Session) NextRunID() string {
	s.runs++
	return fmt.Sprintf("r%d", s.runs)
}

// Allow is a token bucket (rate per second, burst) guarding against abuse.
func (s *Session) Allow(rate, burst float64) bool {
	now := time.Now()
	s.tokens = min(burst, s.tokens+now.Sub(s.tokensAt).Seconds()*rate)
	s.tokensAt = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

// Reset replaces the session world with a freshly seeded playground.
func (s *Session) Reset(cfg sim.Config) error {
	w := sim.NewWorld(cfg, s.NextRunID())
	w.Audit = s.Audit
	for _, t := range []string{
		"doc:42#owner@user:bob",
		"doc:42#viewer@user:alice",
		"group:eng#member@user:carol",
		"folder:7#viewer@group:eng#member",
		"doc:43#parent@folder:7",
	} {
		tuple, err := store.ParseTuple(t)
		if err != nil {
			return err
		}
		if _, _, err := w.Write(store.OpTouch, tuple); err != nil {
			return err
		}
	}
	w.Advance(10_000)
	s.World = w
	return nil
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	max      int
	ttl      time.Duration
}

func NewManager(max int, ttl time.Duration) *Manager {
	m := &Manager{sessions: map[string]*Session{}, max: max, ttl: ttl}
	go func() {
		for range time.Tick(time.Minute) {
			m.expire()
		}
	}()
	return m
}

// Get returns the session for id, creating it if needed (evicting the least
// recently used session when full).
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.lastUsed = time.Now()
		return s, nil
	}
	if len(m.sessions) >= m.max {
		var oldest *Session
		for _, s := range m.sessions {
			if oldest == nil || s.lastUsed.Before(oldest.lastUsed) {
				oldest = s
			}
		}
		delete(m.sessions, oldest.ID)
	}
	s := &Session{ID: id, Audit: &audit.Log{}, lastUsed: time.Now(), tokens: 30, tokensAt: time.Now()}
	if err := s.Reset(sim.DefaultConfig()); err != nil {
		return nil, err
	}
	m.sessions[id] = s
	return s, nil
}

func (m *Manager) expire() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if time.Since(s.lastUsed) > m.ttl {
			delete(m.sessions, id)
		}
	}
}

func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}
