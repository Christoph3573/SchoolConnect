// Package schuelerportal kapselt das Schülerportal
// (https://schueler.schule-infoportal.de, API unter
// https://api.schueler.schule-infoportal.de/<schule>/…).
//
// Login (aus HAR-Mitschnitt rekonstruiert, Laravel-Sanctum-XSRF-Flow;
// <schule> ist das Schulkürzel im Pfad, z.B. "indomagy"):
//  1. GET /<schule>/api/school — bootet die Session, Server setzt
//     XSRF-TOKEN- und schuelerportal_session-Cookies (rotieren pro Response).
//  2. POST /<schule>/login mit JSON {"email","password"} plus Header
//     X-XSRF-TOKEN (= URL-decodierter Wert des XSRF-TOKEN-Cookies).
//     Antwort {"two_factor":false} bei Erfolg.
//  3. Alle weiteren Calls (GET /<schule>/api/user|stundenplan|hausaufgaben|
//     vertretungsplan) mit jeweils aktuellem X-XSRF-TOKEN-Header + Cookies.
//
// Auth gehört der Runtime (siehe domain.Plugin.AuthParams): Das Plugin
// deklariert nur schule/username/password, die Runtime generiert daraus
// "auth"/"logout", speichert erfolgreiche Logins im Session-Store und
// injiziert die Credentials bei jedem Call. Die Datenfunktionen brauchen
// daher keine Credential-Params — egal ob CLI, REST oder MCP aufruft.
package schuelerportal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	coreerrors "schoolconnect/internal/core/errors"
	"schoolconnect/internal/core/tenant"
	"schoolconnect/internal/domain"
)

const (
	apiBase       = "https://api.schueler.schule-infoportal.de"
	webBase       = "https://schueler.schule-infoportal.de/"
	defaultSchule = "indomagy"
)

var schuleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// sessionState hält die Login-Session eines Tenants (eigener Cookie-Jar,
// damit Tenants im selben Prozess keine Sessions teilen).
type sessionState struct {
	client *http.Client
	authed bool
	schule string // Schulkürzel, für das die Session gilt
	email  string // Username/E-Mail, für den die Session gilt
	secret string // Passwort der Session (nur in-memory, für Re-Login bei 401/419)
}

// Plugin implementiert domain.Plugin.
type Plugin struct {
	baseTimeout   time.Duration
	baseTransport http.RoundTripper

	mu       sync.Mutex
	sessions map[string]*sessionState // key: tenant
}

// maxSessions begrenzt die Tenant-Sessions im Speicher (LRU-ähnlich:
// bei Überlauf wird eine beliebige älteste Session verworfen).
const maxSessions = 32

// New erzeugt das Plugin. Der Core-Client liefert Timeout/Transport,
// die Cookie-Verwaltung (Session, pro Tenant) baut das Plugin selbst auf.
func New(client *http.Client) *Plugin {
	timeout := 15 * time.Second
	var transport http.RoundTripper
	if client != nil {
		if client.Timeout > 0 {
			timeout = client.Timeout
		}
		transport = client.Transport
	}
	return &Plugin{
		baseTimeout:   timeout,
		baseTransport: transport,
		sessions:      map[string]*sessionState{},
	}
}

// sessionFor liefert die Session eines Tenants (mit eigenem Cookie-Jar).
func (p *Plugin) sessionFor(ctx context.Context) *sessionState {
	t := tenant.FromContext(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[t]
	if !ok {
		if len(p.sessions) >= maxSessions {
			for k := range p.sessions {
				delete(p.sessions, k)
				break
			}
		}
		jar, _ := cookiejar.New(nil)
		s = &sessionState{client: &http.Client{Timeout: p.baseTimeout, Transport: p.baseTransport, Jar: jar}}
		p.sessions[t] = s
	}
	return s
}

func (p *Plugin) ID() string   { return "schuelerportal" }
func (p *Plugin) Name() string { return "Schülerportal" }
func (p *Plugin) Description() string {
	return "Schülerportal (schule-infoportal.de): Login, Stundenplan, Hausaufgaben und Vertretungsplan."
}

// AuthParams deklariert die Login-Credentials für die Runtime.
// Daraus generiert die Runtime automatisch "auth"/"logout".
func (p *Plugin) AuthParams() []domain.Param {
	return []domain.Param{
		{Name: "schule", Description: "Schulkürzel im API-Pfad, z.B. indomagy.", Required: true, Default: defaultSchule},
		{Name: "username", Description: "Portal-Benutzername (E-Mail).", Required: true, Aliases: []string{"email", "benutzername"}},
		{Name: "password", Description: "Portal-Passwort.", Required: true, Secret: true},
	}
}

// Authenticate baut eine Session auf. Credentials kommen von der Runtime
// (Store + Env + Args, gemergt, Aliase normalisiert) — nur das
// Schulkürzel wird hier noch normalisiert.
func (p *Plugin) Authenticate(ctx context.Context, credentials map[string]string) error {
	c := normalizeCreds(credentials)
	if err := c.validate(); err != nil {
		return coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	return p.login(ctx, c)
}

// EnvCredentials steuert Env-Fallback bei (SCHUELERPORTAL_SECRET:
// "schule:username:passwort", legacy "username:passwort", oder JSON).
func (p *Plugin) EnvCredentials() map[string]string {
	return envCredentials()
}

// --- Functions ---
// Hinweis: keine Credential-Params an Datenfunktionen — die injiziert
// die Runtime aus dem Session-Store (einmal auth, überall angemeldet).

func (p *Plugin) Functions() []domain.Function {
	return []domain.Function{
		{
			Name:        "profil",
			Description: "Eigenes Schülerprofil laden (GET /<schule>/api/user). Prüft auch nur die Session.",
			Handler:     p.profil,
		},
		{
			Name:        "stundenplan",
			Description: "Stundenplan laden (GET /<schule>/api/stundenplan): alle Kurseinträge {day 0-4 (Mo-Fr), hour, uf, room} plus Zeittafel. Enthält alle Kurse der Stufe — ggf. via uf filtern.",
			Params: []domain.Param{
				{Name: "tag", Description: "Optional filtern: 0-4 oder mo/di/mi/do/fr bzw. montag…freitag."},
				{Name: "uf", Description: "Optional filtern nach Kurskürzel (Teiltreffer, z.B. 2m für Mathe-Kurse)."},
			},
			Handler: p.stundenplan,
		},
		{
			Name:        "hausaufgaben",
			Description: "Hausaufgaben laden (GET /<schule>/api/hausaufgaben).",
			Handler:     p.hausaufgaben,
		},
		{
			Name:        "vertretungsplan",
			Description: "Vertretungsplan laden (GET /<schule>/api/vertretungsplan): Einträge {date, hour, class, uf, vertr_uf, room, reason, text, abs_teacher, vertr_teacher} plus Mitteilungen.",
			Params: []domain.Param{
				{Name: "datum", Description: "Optional filtern nach Datum (YYYY-MM-DD)."},
			},
			Handler: p.vertretungsplan,
		},
	}
}

// --- Handler ---

func (p *Plugin) profil(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	var user map[string]any
	if err := p.getAPI(ctx, c.Schule, "/api/user", &user); err != nil {
		return nil, err
	}
	return map[string]any{"schule": c.Schule, "angemeldet_als": c.Username, "profil": user}, nil
}

var wochentagName = map[int]string{0: "Montag", 1: "Dienstag", 2: "Mittwoch", 3: "Donnerstag", 4: "Freitag"}

func (p *Plugin) stundenplan(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	var raw struct {
		Data []struct {
			Day  int    `json:"day"`
			Hour int    `json:"hour"`
			Uf   string `json:"uf"`
			Room string `json:"room"`
		} `json:"data"`
		Zeittafel []struct {
			Hour  int    `json:"hour"`
			Value string `json:"value"`
			Name  string `json:"name"`
		} `json:"zeittafel"`
	}
	if err := p.getAPI(ctx, c.Schule, "/api/stundenplan", &raw); err != nil {
		return nil, err
	}
	zeit := map[int]string{}
	for _, z := range raw.Zeittafel {
		zeit[z.Hour] = z.Value
	}
	tagFilter, err := parseTag(args["tag"])
	if err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	ufFilter := strings.ToLower(strings.TrimSpace(args["uf"]))

	eintraege := []any{}
	for _, e := range raw.Data {
		if tagFilter >= 0 && e.Day != tagFilter {
			continue
		}
		if ufFilter != "" && !strings.Contains(strings.ToLower(e.Uf), ufFilter) {
			continue
		}
		eintraege = append(eintraege, map[string]any{
			"tag": wochentagName[e.Day], "day": e.Day,
			"stunde": e.Hour, "zeit": zeit[e.Hour],
			"kurs": e.Uf, "raum": e.Room,
		})
	}
	return map[string]any{
		"schule": c.Schule, "count": len(eintraege), "eintraege": eintraege, "zeittafel": raw.Zeittafel,
	}, nil
}

func (p *Plugin) hausaufgaben(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	// Antwortform je nach Datenstand: [] oder {"data":[...]} — beides akzeptieren.
	var raw json.RawMessage
	if err := p.getAPI(ctx, c.Schule, "/api/hausaufgaben", &raw); err != nil {
		return nil, err
	}
	aufgaben := []any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &aufgaben); err != nil {
			var wrapped struct {
				Data []any `json:"data"`
			}
			if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
				return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort unverständlich", err)
			}
			aufgaben = wrapped.Data
		}
	}
	if aufgaben == nil {
		aufgaben = []any{}
	}
	return map[string]any{"schule": c.Schule, "count": len(aufgaben), "aufgaben": aufgaben}, nil
}

func (p *Plugin) vertretungsplan(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	var raw struct {
		Data         []map[string]any `json:"data"`
		Mitteilungen []map[string]any `json:"mitteilungen"`
	}
	if err := p.getAPI(ctx, c.Schule, "/api/vertretungsplan", &raw); err != nil {
		return nil, err
	}
	datum := strings.TrimSpace(args["datum"])
	eintraege := []any{}
	for _, e := range raw.Data {
		if datum != "" && fmt.Sprintf("%v", e["date"]) != datum {
			continue
		}
		eintraege = append(eintraege, e)
	}
	if raw.Mitteilungen == nil {
		raw.Mitteilungen = []map[string]any{}
	}
	return map[string]any{
		"schule": c.Schule, "count": len(eintraege), "eintraege": eintraege, "mitteilungen": raw.Mitteilungen,
	}, nil
}

// --- Auth-Mechanik ---

// creds sind die aufgelösten Login-Daten: Schulkürzel + Benutzername + Passwort.
type creds struct {
	Schule   string
	Username string
	Password string
}

func (c creds) validate() error {
	if c.Schule == "" || !schuleRe.MatchString(c.Schule) {
		return fmt.Errorf("ungültiges schulkürzel %q (z.B. indomagy)", c.Schule)
	}
	if c.Username == "" || c.Password == "" {
		return fmt.Errorf(`login braucht schule + username + password — einmal "auth schuelerportal" aufrufen oder als Parameter mitgeben`)
	}
	return nil
}

// normalizeCreds normalisiert gemergte Credentials (Runtime liefert Store +
// Env + Args bereits zusammengeführt, Aliase normalisiert): Schulkürzel
// lowercasen, Default setzen.
func normalizeCreds(args map[string]string) creds {
	schule := strings.ToLower(strings.TrimSpace(args["schule"]))
	if schule == "" {
		schule = defaultSchule
	}
	return creds{
		Schule:   schule,
		Username: strings.TrimSpace(args["username"]),
		Password: args["password"],
	}
}

// envCredentials löst SCHUELERPORTAL_SECRET auf ("schule:username:passwort",
// legacy "username:passwort", oder JSON {"schule","username","password"}).
// Leere Felder bleiben leer — Defaults/Store/Args füllt die Runtime.
func envCredentials() map[string]string {
	secret := strings.TrimSpace(os.Getenv("SCHUELERPORTAL_SECRET"))
	if secret == "" {
		return nil
	}
	out := map[string]string{}
	if strings.HasPrefix(secret, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(secret), &m) == nil {
			if v := strings.ToLower(strings.TrimSpace(firstNonEmpty(m["schule"], m["school"]))); v != "" {
				out["schule"] = v
			}
			if v := strings.TrimSpace(firstNonEmpty(m["username"], m["email"], m["benutzername"])); v != "" {
				out["username"] = v
			}
			if v := firstNonEmpty(m["password"], m["passwort"]); v != "" {
				out["password"] = v
			}
		}
		return out
	}
	if strings.Count(secret, ":") >= 2 {
		parts := strings.SplitN(secret, ":", 3)
		if v := strings.ToLower(strings.TrimSpace(parts[0])); v != "" {
			out["schule"] = v
		}
		if v := strings.TrimSpace(parts[1]); v != "" {
			out["username"] = v
		}
		if parts[2] != "" {
			out["password"] = parts[2]
		}
		return out
	}
	if i := strings.Index(secret, ":"); i > 0 {
		// Legacy ohne Schulkürzel: "username:passwort".
		if v := strings.TrimSpace(secret[:i]); v != "" {
			out["username"] = v
		}
		if secret[i+1:] != "" {
			out["password"] = secret[i+1:]
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ensureAuth meldet an, falls keine Session dieses Tenants für dieses
// Schulkürzel + Benutzer besteht.
func (p *Plugin) ensureAuth(ctx context.Context, c creds) error {
	s := p.sessionFor(ctx)
	p.mu.Lock()
	ok := s.authed && s.schule == c.Schule && strings.EqualFold(s.email, c.Username)
	if ok {
		s.secret = c.Password // Passwortwechsel übernehmen, Session bleibt gültig
	}
	p.mu.Unlock()
	if ok {
		return nil
	}
	return p.login(ctx, c)
}

// login fährt den HAR-rekonstruierten Flow pro Schulkürzel:
// GET /<schule>/api/school (Session-Cookies) → POST /<schule>/login
// (XSRF-Header) → GET /<schule>/api/user (Check).
// Die Session (Jar + State) ist strikt tenant-eigen.
func (p *Plugin) login(ctx context.Context, c creds) error {
	s := p.sessionFor(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()

	base := apiBase + "/" + c.Schule

	// 1. Session booten (setzt XSRF-TOKEN + schuelerportal_session).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/school", nil)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header, "")
	resp, err := s.client.Do(req)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "schuelerportal nicht erreichbar", err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("schuelerportal antwortet mit HTTP %d", resp.StatusCode))
	}

	// 2. Login mit XSRF-Token aus dem Cookie-Jar.
	body, _ := json.Marshal(map[string]string{"email": c.Username, "password": c.Password})
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, base+"/login", bytes.NewReader(body))
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header, xsrfToken(s.client))
	req.Header.Set("Content-Type", "application/json")
	resp, err = s.client.Do(req)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "schuelerportal nicht erreichbar", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == 422 {
		return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(),
			"login abgelehnt (schule/username/passwort prüfen)")
	}
	if resp.StatusCode != http.StatusOK {
		return coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("login antwortet mit HTTP %d", resp.StatusCode))
	}
	var loginResp struct {
		TwoFactor bool `json:"two_factor"`
	}
	if err := json.Unmarshal(raw, &loginResp); err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "login-antwort unverständlich", err)
	}
	if loginResp.TwoFactor {
		return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(),
			"konto verlangt two_factor — wird noch nicht unterstützt")
	}

	// 3. Session verifizieren.
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/user", nil)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header, xsrfToken(s.client))
	resp, err = s.client.Do(req)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "schuelerportal nicht erreichbar", err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session-check nach login fehlgeschlagen")
	}

	s.authed = true
	s.schule = c.Schule
	s.email = c.Username
	s.secret = c.Password
	return nil
}

// getAPI lädt einen JSON-Endpunkt; bei 401/419 (abgelaufene Laravel-Session)
// wird mit den gespeicherten Session-Credentials dieses Tenants einmal neu
// angemeldet und wiederholt.
func (p *Plugin) getAPI(ctx context.Context, schule, path string, out any) error {
	if err := p.doAPI(ctx, schule, path, out); err != nil {
		if ce, ok := err.(*coreerrors.Error); ok && ce.Code == coreerrors.CodeUnauthorized {
			s := p.sessionFor(ctx)
			p.mu.Lock()
			s.authed = false
			c := creds{Schule: s.schule, Username: s.email, Password: s.secret}
			p.mu.Unlock()
			if c.validate() == nil {
				if lerr := p.login(ctx, c); lerr == nil {
					return p.doAPI(ctx, schule, path, out)
				}
			}
		}
		return err
	}
	return nil
}

func (p *Plugin) doAPI(ctx context.Context, schule, path string, out any) error {
	s := p.sessionFor(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/"+schule+path, nil)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header, xsrfToken(s.client))
	resp, err := s.client.Do(req)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "schuelerportal nicht erreichbar", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort lesen fehlgeschlagen", err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.Unmarshal(raw, out); err != nil {
			return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort unverständlich", err)
		}
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == 419:
		return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session abgelaufen (erneut anmelden)")
	default:
		return coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("schuelerportal antwortet mit HTTP %d", resp.StatusCode))
	}
}

// xsrfToken liest den aktuellen XSRF-TOKEN-Cookie-Wert (URL-decodiert,
// wie ihn der Browser als X-XSRF-TOKEN-Header mitschickt).
func xsrfToken(client *http.Client) string {
	u, _ := url.Parse(apiBase + "/")
	for _, c := range client.Jar.Cookies(u) {
		if c.Name != "XSRF-TOKEN" {
			continue
		}
		if dec, err := url.QueryUnescape(c.Value); err == nil {
			return dec
		}
		return c.Value
	}
	return ""
}

// setBrowserHeaders setzt die Header, die das Portal laut HAR erwartet.
func setBrowserHeaders(h http.Header, xsrf string) {
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Referer", webBase)
	if xsrf != "" {
		h.Set("X-XSRF-TOKEN", xsrf)
	}
	if _, ok := h["Origin"]; !ok && h.Get("Content-Type") != "" {
		h.Set("Origin", strings.TrimSuffix(webBase, "/"))
	}
	h.Set("User-Agent", "SchoolConnect/1.0")
}

// parseTag löst Tagfilter auf: "" → -1 (alle), 0-4, mo/di/mi/do/fr,
// montag…freitag. day 0 = Montag (Konvention aus HAR: 5 Tage 0-4).
func parseTag(in string) (int, error) {
	s := strings.ToLower(strings.TrimSpace(in))
	if s == "" {
		return -1, nil
	}
	namen := map[string]int{
		"0": 0, "mo": 0, "montag": 0, "monday": 0,
		"1": 1, "di": 1, "dienstag": 1, "tuesday": 1,
		"2": 2, "mi": 2, "mittwoch": 2, "wednesday": 2,
		"3": 3, "do": 3, "donnerstag": 3, "thursday": 3,
		"4": 4, "fr": 4, "freitag": 4, "friday": 4,
	}
	if d, ok := namen[s]; ok {
		return d, nil
	}
	return -1, fmt.Errorf("unbekannter tag %q (0-4 oder mo/di/mi/do/fr)", in)
}
