// Package session verwaltet Auth-Sessions pro Plugin (In-Memory-Stub).
package session

import (
	"sync"
	"time"
)

// Session ist eine Plugin-Sitzung.
type Session struct {
	PluginID  string
	Token     string
	CreatedAt time.Time
}

// Store hält Sessions pro Plugin-ID (thread-safe).
type Store struct {
	mu       sync.RWMutex
	sessions map[string]Session
}

// New erzeugt einen leeren Store.
func New() *Store { return &Store{sessions: map[string]Session{}} }

// Set legt eine Session ab.
func (s *Store) Set(pluginID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[pluginID] = Session{PluginID: pluginID, Token: token, CreatedAt: time.Now()}
}

// Get liest eine Session (ok=false wenn keine existiert).
func (s *Store) Get(pluginID string) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ses, ok := s.sessions[pluginID]
	return ses, ok
}
