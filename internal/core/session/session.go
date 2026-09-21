// Package session verwaltet Plugin-Credentials pro Tenant und Plugin-ID.
//
// Ablage ist eine JSON-Datei mit 0600-Rechten in einem 0700-Verzeichnis,
// damit ein einmaliges `auth` prozessübergreifend gilt: CLI, REST und MCP
// teilen sich denselben Store. Das Format ist
// {"<tenant>": {"<plugin-id>": {"key": "value", ...}}}; ein Alt-Format
// ohne Tenant-Ebene ({"<plugin-id>": {...}}) wird einmalig unter den
// Legacy-Tenant "_legacy" migriert. Sensible Werte (Param.Secret) werden
// nie geloggt — nur hier abgelegt.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// LegacyTenant nimmt Credentials aus dem Alt-Format ohne Tenant-Ebene auf.
const LegacyTenant = "_legacy"

// Store hält Credentials je Tenant und Plugin-ID (thread-safe, file-backed).
// Das Format ist {"<tenant>": {"<plugin-id>": {"key": "value", ...}}}.
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

// Get liest die Credentials eines Plugins für einen Tenant
// (ok=false wenn keine existieren).
// Jede Get liest von Platte (read-through), damit parallele Prozesse
// (CLI-Aufrufe, REST-Server, MCP-Server) denselben Stand sehen.
func (s *Store) Get(tenant, pluginID string) (map[string]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.readLocked()
	m, ok := all[normalizeTenant(tenant)][pluginID]
	if !ok || len(m) == 0 {
		return nil, false
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out, true
}

// Set legt die Credentials eines Plugins für einen Tenant ab
// (leere Werte werden verworfen). Der Gesamtstand (inkl. migrierter
// _legacy-Einträge) wird im neuen Format zurückgeschrieben.
func (s *Store) Set(tenant, pluginID string, creds map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readLocked()
	clean := make(map[string]string, len(creds))
	for k, v := range creds {
		if v != "" {
			clean[k] = v
		}
	}
	t := normalizeTenant(tenant)
	if all[t] == nil {
		all[t] = map[string]map[string]string{}
	}
	all[t][pluginID] = clean
	return s.writeLocked(all)
}

// Delete verwirft die Credentials eines Plugins für einen Tenant (logout).
// Nicht existierende Einträge sind kein Fehler. Nach einer Änderung wird
// der Gesamtstand (inkl. migrierter _legacy-Einträge) zurückgeschrieben,
// damit die Datei nach dem ersten Schreibzugriff im neuen Format vorliegt.
func (s *Store) Delete(tenant, pluginID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readLocked()
	changed := false
	t := normalizeTenant(tenant)
	if _, ok := all[t][pluginID]; ok {
		delete(all[t], pluginID)
		if len(all[t]) == 0 {
			delete(all, t)
		}
		changed = true
	}
	if _, ok := all[LegacyTenant]; ok {
		changed = true // Alt-Format einmalig ins neue Format überführen
	}
	if !changed {
		return nil
	}
	return s.writeLocked(all)
}

// normalizeTenant fällt auf den Default-Tenant zurück.
func normalizeTenant(tenant string) string {
	if tenant == "" {
		return "default"
	}
	return tenant
}

// readLocked liest den Gesamtstand (fehlende Datei = leer, kein Fehler).
// Ein Alt-Format ohne Tenant-Ebene ({"<plugin-id>": {"k": "v"}}) wird
// erkannt — json.Unmarshal ins neue Format schlägt dafür fehl bzw. liefert
// nil-Maps — und unter dem Legacy-Tenant "_legacy" einsortiert.
func (s *Store) readLocked() map[string]map[string]map[string]string {
	all := map[string]map[string]map[string]string{}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return all
	}
	if err := json.Unmarshal(raw, &all); err == nil && !hasNilLeaf(all) {
		if all == nil {
			all = map[string]map[string]map[string]string{}
		}
		return all
	}
	all = map[string]map[string]map[string]string{}
	if legacy := sniffLegacyFormat(raw); legacy != nil {
		all[LegacyTenant] = legacy
	}
	return all
}

// hasNilLeaf erkennt einen fehlgeschlagenen Unmarshal ins 3-Ebenen-Format:
// json setzt dabei leere (nil) Maps auf der innersten Ebene.
func hasNilLeaf(all map[string]map[string]map[string]string) bool {
	for _, plugins := range all {
		for _, creds := range plugins {
			if creds == nil {
				return true
			}
		}
	}
	return false
}

// sniffLegacyFormat erkennt das Alt-Format {"<plugin-id>": {"k": "v"}}
// und liefert es zurück; sonst nil. Das neue Format
// {"<tenant>": {"<plugin-id>": {...}}} ergibt konsequent
// map[string]any-Werte auf der zweiten Ebene und wird nicht verwechselt.
func sniffLegacyFormat(raw []byte) map[string]map[string]string {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe) == 0 {
		return nil
	}
	out := make(map[string]map[string]string, len(probe))
	for pluginID, v := range probe {
		var creds map[string]string
		if err := json.Unmarshal(v, &creds); err != nil {
			return nil // zweite Ebene ist kein String-Map → neues Format
		}
		out[pluginID] = creds
	}
	return out
}

// writeLocked schreibt den Gesamtstand (Verzeichnis 0700, Datei 0600).
func (s *Store) writeLocked(all map[string]map[string]map[string]string) error {
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
