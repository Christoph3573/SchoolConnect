// Package tenant stellt die Tenant-Dimension für den Multi-Tenant-Betrieb.
//
// Ein Tenant trennt Credentials (Session-Store) und Login-Sessions
// (Plugin-Jars/Tokens) pro Nutzer/Gruppe. Der Tenant steht nie als
// domain.Param in Funktions-Args, sondern reist im context.Context mit:
// Adapter (REST-Header, CLI-Flag/Env, MCP-Env) → Runtime → Plugin.
// Default-Tenant ist "default", sodass Einmal-Nutzer ohne Tenant-Angabe
// exakt wie bisher arbeiten.
package tenant

import (
	"context"
	"os"
	"regexp"
	"strings"
)

// Default ist der Tenant für normale Einzelnutzung (CLI/MCP ohne Angabe).
const Default = "default"

// Header ist der REST-Header für den Tenant.
const Header = "X-SC-Tenant"

// SigHeader trägt den optionalen HMAC über den Tenant (Hex).
const SigHeader = "X-SC-Tenant-Sig"

var validRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-_]{0,63}$`)

type ctxKey struct{}

// Normalize normalisiert und validiert einen Tenant-Namen:
// leer → Default, sonst lower-case + Zeichensatzprüfung.
func Normalize(s string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return Default, nil
	}
	if !validRe.MatchString(t) {
		return "", &Error{Tenant: s}
	}
	return t, nil
}

// MustNormalize wie Normalize, fällt bei ungültigem Wert auf Default zurück.
func MustNormalize(s string) string {
	t, err := Normalize(s)
	if err != nil {
		return Default
	}
	return t
}

// Error meldet einen ungültigen Tenant-Namen.
type Error struct{ Tenant string }

func (e *Error) Error() string {
	return "ungültiger tenant " + quote(e.Tenant) + " (a-z, 0-9, -, _; max. 64 Zeichen)"
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	return `"` + s + `"`
}

// WithTenant legt den Tenant in den Context.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, ctxKey{}, MustNormalize(tenant))
}

// FromContext liest den Tenant aus dem Context (Default wenn keiner gesetzt).
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return Default
	}
	if t, ok := ctx.Value(ctxKey{}).(string); ok && t != "" {
		return t
	}
	return Default
}

// FromEnv liest den Tenant aus SC_TENANT ("" wenn nicht gesetzt/ungültig).
func FromEnv() string {
	t := strings.TrimSpace(os.Getenv("SC_TENANT"))
	if t == "" {
		return ""
	}
	n, err := Normalize(t)
	if err != nil {
		return ""
	}
	return n
}

// FromEnvOrDefault wie FromEnv, fällt auf Default zurück.
func FromEnvOrDefault() string {
	if t := FromEnv(); t != "" {
		return t
	}
	return Default
}
