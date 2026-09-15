// Package session verwaltet Plugin-Credentials pro Plugin-ID.
//
// Ablage ist eine JSON-Datei mit 0600-Rechten in einem 0700-Verzeichnis,
// damit ein einmaliges `auth` prozessübergreifend gilt: CLI, REST und MCP
// teilen sich denselben Store. Sensible Werte (Param.Secret) werden nie
// geloggt — nur hier abgelegt.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Store hält Credentials je Plugin-ID (thread-safe, file-backed).
// Das Format ist {"<plugin-id>": {"key": "value", ...}}.
type Store struct {
	mu   sync.RWMutex
	path string
}

// DefaultPath liefert den Ablagepfad:
// $SCHOOLCONNECT_CONFIG_DIR/credentials.json oder
// ~/.config/schoolconnect/credentials.json.
func DefaultPath() string {
	if dir := os.Getenv("SCHOOLCONNECT_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "credentials.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".config", "schoolconnect", "credentials.json")
}

// New erzeugt einen Store am DefaultPath.
func New() *Store { return &Store{path: DefaultPath()} }

// NewAtPath erzeugt einen Store an einem expliziten Pfad (für Tests).
func NewAtPath(path string) *Store { return &Store{path: path} }

// Path gibt den Ablagepfad zurück.
func (s *Store) Path() string { return s.path }

// Get liest die Credentials eines Plugins (ok=false wenn keine existieren).
// Jede Get liest von Platte (read-through), damit parallele Prozesse
// (CLI-Aufrufe, REST-Server, MCP-Server) denselben Stand sehen.
func (s *Store) Get(pluginID string) (map[string]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.readLocked()
	m, ok := all[pluginID]
	if !ok || len(m) == 0 {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out, true
}

// Set legt die Credentials eines Plugins ab (leere Werte werden verworfen).
func (s *Store) Set(pluginID string, creds map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readLocked()
	clean := make(map[string]string, len(creds))
	for k, v := range creds {
		if v != "" {
			clean[k] = v
		}
	}
	all[pluginID] = clean
	return s.writeLocked(all)
}

// Delete verwirft die Credentials eines Plugins (logout).
// Nicht existierende Einträge sind kein Fehler.
func (s *Store) Delete(pluginID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readLocked()
	if _, ok := all[pluginID]; !ok {
		return nil
	}
	delete(all, pluginID)
	return s.writeLocked(all)
}

// readLocked liest den Gesamtstand (fehlende Datei = leer, kein Fehler).
func (s *Store) readLocked() map[string]map[string]string {
	all := map[string]map[string]string{}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return all
	}
	_ = json.Unmarshal(raw, &all)
	if all == nil {
		all = map[string]map[string]string{}
	}
	return all
}

// writeLocked schreibt den Gesamtstand (Verzeichnis 0700, Datei 0600).
func (s *Store) writeLocked(all map[string]map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
