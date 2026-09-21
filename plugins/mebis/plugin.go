// Package mebis kapselt die ByCS-Lernplattform (Moodle unter
// https://lernplattform.bycs.de).
//
// Login (aus HAR-Mitschnitten rekonstruiert, live per curl verifiziert):
//  1. GET / (Cookie-Jar) — Moodle redirectet unauthentifiziert über
//     /login/index.php → /local/bycsauth/login.php → /auth/oauth2/login.php
//     (Issuer 4) nach auth.bycs.de/.../openid-connect/auth
//     (?client_id=lernplattform-v2&response_type=code, PKCE via Moodle).
//  2. Dort liegt ein Keycloak-Loginformular (SELECTOR unten):
//     POST auf die Form-Action mit
//     application/x-www-form-urlencoded {username, password, credentialId:""}.
//  3. Bei Erfolg 302 zurück auf /admin/oauth2callback.php?code=…&state=…,
//     Moodle löst code+PKCE ein und setzt MoodleSession (+ sesskey im Seiten-HTML).
//  4. Alle Daten-Calls brauchen MoodleSession-Cookie + sesskey
//     (AJAX: POST /lib/ajax/service.php?sesskey=…&info=<wsfunction> mit
//     JSON-Body [{index, methodname, args}], Envelope [{error, data}]).
//
// Dateien (fetch): Module vom Typ "Datei" (resource) liefern nur
// /mod/resource/view.php?id=<cmid>; der Download läuft über dieselbe
// Session (ggf. ?redirect=1 bzw. dem pluginfile-Link auf der
// resourceworkaround-Seite folgen). Verzeichnisse (folder) listen ihre
// Dateien — eine davon per datei-Teiltreffer wählen.
//
// Auth gehört der Runtime (siehe domain.Plugin.AuthParams): Das Plugin
// deklariert nur username/password, die Runtime generiert daraus
// "auth"/"logout", speichert erfolgreiche Logins im Session-Store und
// injiziert die Credentials bei jedem Call. Die Datenfunktionen brauchen
// daher keine Credential-Params — egal ob CLI, REST oder MCP aufruft.
package mebis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	coreerrors "schoolconnect/internal/core/errors"
	"schoolconnect/internal/core/tenant"
	"schoolconnect/internal/domain"
)

const (
	moodleBase  = "https://lernplattform.bycs.de"
	maxBodyRead = 8 << 20
	maxDownload = 256 << 20
)

// formRe findet die Keycloak-Loginform-Action (einzige POST-Form auf der Seite).
var formRe = regexp.MustCompile(`(?s)<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)

// sesskeyRe findet den AJAX-Schlüssel im Moodle-Seiten-HTML.
var sesskeyRe = regexp.MustCompile(`"sesskey":"([^"]+)"`)

// pluginfileLinkRe findet Datei-Links (pluginfile.php) in Modulseiten.
var pluginfileLinkRe = regexp.MustCompile(`(?s)<a[^>]+href="([^"]*pluginfile\.php[^"]*)"[^>]*>(.*?)</a>`)

// workaroundRe grenzt den Datei-Link einer resource-Seite ein
// (<div class="resourceworkaround"><a href="…pluginfile…">).
var workaroundRe = regexp.MustCompile(`(?s)<div[^>]*class="[^"]*resourceworkaround[^"]*"[^>]*>(.*?)</div>`)

// folderRe erkennt Verzeichnis-Seiten (mehrere Dateien statt Direkt-Download).
var folderRe = regexp.MustCompile(`foldertree|mod-folder|fp-filename`)

// tagRe/wsRe bereinigen Link-Texte zu Dateinamen.
var tagRe = regexp.MustCompile(`<[^>]+>`)
var wsRe = regexp.MustCompile(`\s+`)

// dispNameRe liest den Dateinamen aus Content-Disposition.
var dispNameRe = regexp.MustCompile(`(?i)filename\*=UTF-8''([^;]+)|filename="([^"]+)"`)

// journalFormRe findet das Tagebuch-Eintragsformular auf mod/journal/edit.php.
var journalFormRe = regexp.MustCompile(`(?s)<form[^>]*action="([^"]*mod/journal/edit\.php)"[^>]*>(.*?)</form>`)

// journalInputTagRe/journalAttrRe zerlegen die Hidden-Felder des Formulars
// (Attribut-Reihenfolge variiert je Feld, siehe extractHiddenFields).
var journalInputTagRe = regexp.MustCompile(`<input\b[^>]*/?>`)
var journalAttrRe = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.\[\]]*)\s*=\s*"([^"]*)"`)

var numRe = regexp.MustCompile(`^[0-9]+$`)
var typRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// sessionState hält die Login-Session eines Tenants (eigener Cookie-Jar,
// damit Tenants im selben Prozess keine Sessions teilen).
type sessionState struct {
	client  *http.Client // eigener Client mit Cookie-Jar für die Moodle-Session
	authed  bool
	user    string // ByCS-Kennung, für die die Session gilt
	secret  string // Passwort der Session (nur in-memory, für Re-Login bei 403)
	sesskey string // Moodle-AJAX-Schlüssel der Session
}

// maxSessions begrenzt die Tenant-Sessions im Speicher.
const maxSessions = 32

// Plugin implementiert domain.Plugin.
type Plugin struct {
	baseTimeout   time.Duration
	baseTransport http.RoundTripper

	mu       sync.Mutex
	sessions map[string]*sessionState // key: tenant
}

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

func (p *Plugin) ID() string   { return "mebis" }
func (p *Plugin) Name() string { return "mebis" }
func (p *Plugin) Description() string {
	return "ByCS-Lernplattform (Moodle): Login, Kurse, Kursinhalte und Datei-Download."
}

// AuthParams deklariert die Login-Credentials für die Runtime.
// Daraus generiert die Runtime automatisch "auth"/"logout".
func (p *Plugin) AuthParams() []domain.Param {
	return []domain.Param{
		{Name: "username", Description: "ByCS-Kennung.", Required: true, Aliases: []string{"benutzername", "kennung"}},
		{Name: "password", Description: "ByCS-Passwort.", Required: true, Secret: true},
	}
}

// Authenticate baut eine Session auf. Credentials kommen von der Runtime
// (Store + Env + Args, gemergt, Aliase normalisiert).
func (p *Plugin) Authenticate(ctx context.Context, credentials map[string]string) error {
	c := normalizeCreds(credentials)
	if err := c.validate(); err != nil {
		return coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	return p.login(ctx, c)
}

// EnvCredentials steuert Env-Fallback bei (MEBIS_SECRET:
// "username:passwort" oder JSON {"username","password"}).
func (p *Plugin) EnvCredentials() map[string]string {
	return envCredentials()
}

// --- Functions ---
// Hinweis: keine Credential-Params an Datenfunktionen — die injiziert
// die Runtime aus dem Session-Store (einmal auth, überall angemeldet).

func (p *Plugin) Functions() []domain.Function {
	return []domain.Function{
		{
			Name:        "courses",
			Description: "Belegte Kurse auflisten (Moodle-WS core_course_get_enrolled_courses_by_timeline_classification).",
			Params: []domain.Param{
				{Name: "query", Description: "Optional filtern nach Kursname/Kurzname (Teiltreffer, case-insensitiv)."},
			},
			Handler: p.courses,
		},
		{
			Name:        "abschnitte",
			Description: "Abschnittsübersicht eines Kurses (leichtgewichtig): Nr, Titel, Modulanzahl, Modultypen. Einstieg für inhalt/fetch.",
			Params: []domain.Param{
				{Name: "course", Description: "Kurs-ID (siehe courses).", Required: true},
				{Name: "query", Description: "Optional filtern nach Abschnittstitel (Teiltreffer, case-insensitiv)."},
			},
			Handler: p.abschnitte,
		},
		{
			Name:        "inhalt",
			Description: "Kursinhalt laden: Abschnitte + Module mit Typ, Name und URL (Moodle-WS core_courseformat_get_state). Nutze course aus courses.",
			Params: []domain.Param{
				{Name: "course", Description: "Kurs-ID (siehe courses).", Required: true},
				{Name: "abschnitt", Description: "Optional: nur Abschnitte mit diesem Titel-Teiltreffer."},
				{Name: "typ", Description: "Optional: nur Module dieses Typs (z.B. Datei, resource, H5P, Textseite, Forum)."},
				{Name: "query", Description: "Optional: nur Module mit diesem Namen-Teiltreffer."},
			},
			Handler: p.inhalt,
		},
		{
			Name:        "fetch",
			Description: "Datei aus einem Kursmodul laden (Datei/resource) und lokal speichern. Verzeichnisse (folder) listen ihre Dateien — dann per datei eine wählen.",
			Params: []domain.Param{
				{Name: "modul", Description: "Kursmodul-ID aus inhalt (z.B. 85090819). Alternativ url angeben.", Aliases: []string{"id", "cmid", "module"}},
				{Name: "url", Description: "Volle Modul-URL als Alternative zu modul (z.B. …/mod/resource/view.php?id=…)."},
				{Name: "typ", Description: "Modul-Typ für den URL-Aufbau (resource, folder, …).", Default: "resource"},
				{Name: "datei", Description: "Bei mehreren Dateien (Verzeichnis): Teiltreffer auf Dateiname wählen.", Aliases: []string{"name", "dateiname"}},
				{Name: "ziel", Description: "Zielpfad (Datei oder Ordner). Default: aktueller Ordner + Dateiname.", Aliases: []string{"ausgabe", "pfad", "output"}},
			},
			Handler: p.fetch,
		},
		{
			Name:        "eintrag",
			Description: "Tagebucheintrag (Aktivität Tagebuch/mod_journal) schreiben oder aktualisieren.",
			Params: []domain.Param{
				{Name: "modul", Description: "Kursmodul-ID des Tagebuchs aus inhalt (typ Tagebuch).", Required: true, Aliases: []string{"id", "cmid"}},
				{Name: "text", Description: "Eintragstext.", Required: true},
				{Name: "format", Description: "Textformat: html (Absätze werden automatisch erzeugt) oder plain.", Default: "html"},
			},
			Handler: p.eintrag,
		},
		{
			Name:        "h5p-abschliessen",
			Description: "H5P-Zuordnungsaufgabe (H5P.DragQuestion) mit perfekter Musterlösung abschließen (setFinished-Bewertung senden). Andere H5P-Typen werden abgelehnt.",
			Params: []domain.Param{
				{Name: "modul", Description: "Kursmodul-ID der H5P-Aktivität aus inhalt (typ H5P).", Required: true, Aliases: []string{"id", "cmid"}},
			},
			Handler: p.h5pAbschliessen,
		},
	}
}

// --- Handler ---

func (p *Plugin) courses(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	var data struct {
		Courses []struct {
			ID          int    `json:"id"`
			Fullname    string `json:"fullname"`
			Shortname   string `json:"shortname"`
			ViewURL     string `json:"viewurl"`
			CourseImage string `json:"courseimage"`
			Progress    any    `json:"progress"`
			HasProgress bool   `json:"hasprogress"`
			CourseCat   string `json:"coursecategory"`
			Startdate   int64  `json:"startdate"`
			Enddate     int64  `json:"enddate"`
			Visible     bool   `json:"visible"`
			Hidden      bool   `json:"hidden"`
			IsFavourite bool   `json:"isfavourite"`
		} `json:"courses"`
		NextOffset int `json:"nextoffset"`
	}
	if err := p.ajax(ctx, "core_course_get_enrolled_courses_by_timeline_classification", map[string]any{
		"offset": 0, "limit": 100, "classification": "all", "sort": "fullname",
		"customfieldname": "schule", "customfieldvalue": "",
		"requiredfields": []string{"id", "fullname", "shortname", "showcoursecategory", "showshortname", "visible", "enddate"},
	}, &data); err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(args["query"]))
	out := []any{}
	for _, k := range data.Courses {
		if q != "" && !strings.Contains(strings.ToLower(k.Fullname), q) &&
			!strings.Contains(strings.ToLower(k.Shortname), q) {
			continue
		}
		out = append(out, map[string]any{
			"course": k.ID, "titel": strings.TrimSpace(k.Fullname), "kurzname": strings.TrimSpace(k.Shortname),
			"url": k.ViewURL, "kategorie": k.CourseCat, "fortschritt": k.Progress,
			"favorit": k.IsFavourite, "sichtbar": k.Visible && !k.Hidden,
		})
	}
	return map[string]any{"angemeldet_als": c.Username, "count": len(out), "kurse": out}, nil
}

// abschnitte liefert die leichte Übersicht: Nr, Titel, Modulanzahl, Modultypen.
func (p *Plugin) abschnitte(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	course := strings.TrimSpace(args["course"])
	if course == "" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), `missing required param "course" (siehe courses)`)
	}
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	state, err := p.loadCourseState(ctx, course)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(args["query"]))
	out := []any{}
	for _, s := range state.Section {
		titel := sectionTitle(s)
		if q != "" && !strings.Contains(strings.ToLower(titel), q) {
			continue
		}
		n := 0
		typen := []string{}
		seen := map[string]bool{}
		for _, m := range state.CM {
			if m.Section != s.Number {
				continue
			}
			n++
			if m.Modname != "" && !seen[m.Modname] {
				seen[m.Modname] = true
				typen = append(typen, m.Modname)
			}
		}
		out = append(out, map[string]any{
			"nr": s.Number, "titel": titel, "sichtbar": s.Visible,
			"module": n, "typen": typen,
		})
	}
	return map[string]any{"course": course, "count": len(out), "abschnitte": out}, nil
}

func (p *Plugin) inhalt(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	course := strings.TrimSpace(args["course"])
	if course == "" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), `missing required param "course" (siehe courses)`)
	}
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	state, err := p.loadCourseState(ctx, course)
	if err != nil {
		return nil, err
	}
	abschnittQ := strings.ToLower(strings.TrimSpace(args["abschnitt"]))
	typQ := strings.ToLower(strings.TrimSpace(args["typ"]))
	queryQ := strings.ToLower(strings.TrimSpace(args["query"]))

	bySection := map[int][]any{}
	countModule := 0
	for _, m := range state.CM {
		if typQ != "" && !strings.Contains(strings.ToLower(m.Modname), typQ) &&
			!strings.Contains(strings.ToLower(m.Module), typQ) {
			continue
		}
		if queryQ != "" && !strings.Contains(strings.ToLower(m.Name), queryQ) {
			continue
		}
		bySection[m.Section] = append(bySection[m.Section], map[string]any{
			"id": m.ID, "name": m.Name, "typ": m.Modname, "modul": m.Module,
			"url": moduleViewURL(m), "sichtbar": m.Visible,
		})
		countModule++
	}
	abschnitte := []any{}
	for _, s := range state.Section {
		titel := sectionTitle(s)
		if abschnittQ != "" && !strings.Contains(strings.ToLower(titel), abschnittQ) {
			continue
		}
		module := bySection[s.Number]
		if module == nil {
			// Mit Modul-Filter (typ/query) leere Abschnitte ausblenden —
			// ohne Filter alle zeigen (Orientierung wie im Kurs).
			if typQ != "" || queryQ != "" {
				continue
			}
			module = []any{}
		}
		abschnitte = append(abschnitte, map[string]any{
			"nr": s.Number, "titel": titel, "sichtbar": s.Visible, "module": module,
		})
	}
	return map[string]any{
		"course": course, "count_abschnitte": len(abschnitte),
		"count_module": countModule, "abschnitte": abschnitte,
	}, nil
}

// fetch lädt eine Datei aus einem Kursmodul und speichert sie lokal.
// Bei Verzeichnis-Modulen (mehrere Dateien) wird ohne datei-Treffer die
// Dateiliste zurückgegeben statt zu raten.
func (p *Plugin) fetch(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	viewURL := strings.TrimSpace(args["url"])
	modul := strings.TrimSpace(firstNonEmpty(args["modul"], args["id"], args["cmid"], args["module"]))
	typ := strings.ToLower(strings.TrimSpace(args["typ"]))
	if typ == "" {
		typ = "resource"
	}
	if !typRe.MatchString(typ) {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			fmt.Sprintf("ungültiger typ %q (z.B. resource, folder)", args["typ"]))
	}
	built := false
	if viewURL == "" {
		if modul == "" {
			return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
				`entweder "modul" (Kursmodul-ID aus inhalt) oder "url" angeben`)
		}
		if !numRe.MatchString(modul) {
			return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
				fmt.Sprintf("ungültige modul-id %q (Zahl aus inhalt erwartet)", modul))
		}
		viewURL = moodleBase + "/mod/" + typ + "/view.php?id=" + modul
		built = true
	} else if modul == "" {
		modul = cmidFromURL(viewURL)
	}

	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}

	// 1. Versuch: Direkt-Download (resource mit ?redirect=1 lädt sofort).
	tryURL := viewURL
	if built && typ == "resource" {
		tryURL = viewURL + "&redirect=1"
	}
	final, ctype, disp, body, err := p.getBinary(ctx, tryURL)
	if err != nil {
		return nil, err
	}
	if !isHTML(ctype, body) {
		return p.saveDownload(args, modul, typ, final, ctype, disp, body)
	}

	// 2. HTML-Seite: Datei-Link(s) suchen.
	page := string(body)
	if w := workaroundRe.FindStringSubmatch(page); w != nil {
		if m := pluginfileLinkRe.FindStringSubmatch(w[1]); m != nil {
			link := absolutize(final, html.UnescapeString(m[1]))
			_, ctype2, disp2, body2, err := p.getBinary(ctx, link)
			if err != nil {
				return nil, err
			}
			if isHTML(ctype2, body2) {
				return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "datei-link führt nicht auf eine Datei")
			}
			return p.saveDownload(args, modul, typ, link, ctype2, disp2, body2)
		}
	}
	links := fileLinks(final, page)
	if len(links) == 0 {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			fmt.Sprintf("kein Datei-Link auf der Modulseite (typ=%s) — fetch lädt Datei-Module (resource); Verzeichnisse listen mit typ=folder", typ))
	}
	if typ != "resource" && typ != "folder" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			fmt.Sprintf("modul ist kein Datei-Modul (typ=%s) — %d Datei-Link(s) gefunden, Abbruch statt Fehl-Download", typ, len(links)))
	}
	want := strings.ToLower(strings.TrimSpace(firstNonEmpty(args["datei"], args["name"], args["dateiname"])))
	if want != "" {
		treffer := []fileLink{}
		for _, l := range links {
			if strings.Contains(strings.ToLower(l.Name), want) {
				treffer = append(treffer, l)
			}
		}
		if len(treffer) == 0 {
			return nil, coreerrors.New(coreerrors.CodeNotFound, p.ID(),
				fmt.Sprintf("keine Datei für %q (Modul %s listet %d Dateien)", args["datei"], modulOrURL(modul, viewURL), len(links)))
		}
		if len(treffer) > 1 {
			return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
				fmt.Sprintf("%q trifft %d Dateien — bitte eindeutiger wählen", args["datei"], len(treffer)))
		}
		_, ctype2, disp2, body2, err := p.getBinary(ctx, treffer[0].URL)
		if err != nil {
			return nil, err
		}
		if isHTML(ctype2, body2) {
			return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "datei-link führt nicht auf eine Datei")
		}
		return p.saveDownload(args, modul, typ, treffer[0].URL, ctype2, disp2, body2)
	}
	if len(links) == 1 && typ == "resource" {
		_, ctype2, disp2, body2, err := p.getBinary(ctx, links[0].URL)
		if err != nil {
			return nil, err
		}
		if isHTML(ctype2, body2) {
			return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "datei-link führt nicht auf eine Datei")
		}
		return p.saveDownload(args, modul, typ, links[0].URL, ctype2, disp2, body2)
	}
	// Mehrere Dateien (typischerweise folder): auflisten statt raten.
	dateien := []any{}
	for _, l := range links {
		dateien = append(dateien, map[string]any{"name": l.Name, "url": l.URL})
	}
	return map[string]any{
		"modul": modulOrURL(modul, viewURL), "typ": typ, "url": viewURL,
		"verzeichnis": true,
		"hinweis":     "mehrere Dateien — mit --datei <name> eine wählen (Teiltreffer)",
		"count":       len(dateien), "dateien": dateien,
	}, nil
}

// eintrag schreibt/aktualisiert einen Tagebuch-Eintrag (mod_journal).
// Moodle-Formulare haben keinen stabilen Webservice für journal — der Flow
// lädt daher edit.php, liest sesskey/itemid/format aus dem echten Formular
// und postet es mit dem neuen Text zurück (wie ein Browser-Submit).
func (p *Plugin) eintrag(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	modul := strings.TrimSpace(firstNonEmpty(args["modul"], args["id"], args["cmid"]))
	if modul == "" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			`missing required param "modul" (Kursmodul-ID des Tagebuchs, siehe inhalt)`)
	}
	if !numRe.MatchString(modul) {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			fmt.Sprintf("ungültige modul-id %q (Zahl aus inhalt erwartet)", modul))
	}
	text := args["text"]
	if strings.TrimSpace(text) == "" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), `missing required param "text"`)
	}
	format := strings.ToLower(strings.TrimSpace(args["format"]))
	if format == "" {
		format = "html"
	}
	if format != "html" && format != "plain" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			fmt.Sprintf("ungültiges format %q (html oder plain)", args["format"]))
	}

	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}

	editURL := moodleBase + "/mod/journal/edit.php?id=" + modul
	_, page, err := p.getPage(ctx, editURL)
	if err != nil {
		return nil, err
	}
	fm := journalFormRe.FindStringSubmatch(page)
	if fm == nil {
		if strings.Contains(page, `id="kc-form-login"`) {
			return nil, coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session abgelaufen (erneut anmelden)")
		}
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			"kein Tagebuch-Formular gefunden (modul-id prüfen — ist es ein Tagebuch/journal?)")
	}
	action := html.UnescapeString(fm[1])
	formBody := fm[2]
	fields := extractHiddenFields(formBody)
	if fields["sesskey"] == "" || fields["id"] == "" {
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "formular unvollständig (id/sesskey fehlen)")
	}
	submitVal := submitButtonValue(formBody, "submitbutton")
	if submitVal == "" {
		submitVal = "Änderungen speichern"
	}

	content := text
	fmtVal := fields["text_editor[format]"]
	if format == "plain" {
		fmtVal = "2" // FORMAT_PLAIN
	} else {
		if fmtVal == "" {
			fmtVal = "1" // FORMAT_HTML
		}
		if !strings.Contains(content, "<") {
			content = wrapParagraphs(content)
		}
	}

	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	form.Set("text_editor[text]", content)
	form.Set("text_editor[format]", fmtVal)
	form.Set("submitbutton", submitVal)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.httpClient(ctx).Do(req)
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "lernplattform nicht erreichbar", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	resp.Body.Close()
	finalURL := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	resultPage := string(raw)
	if strings.Contains(resultPage, `id="kc-form-login"`) {
		return nil, coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session abgelaufen (erneut anmelden)")
	}
	if strings.Contains(finalURL, "/mod/journal/edit.php") {
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			"eintrag wurde nicht übernommen (Formular zeigt Fehler — Text prüfen)")
	}
	return map[string]any{
		"modul": modul, "gespeichert": true, "format": format,
		"url": moodleBase + "/mod/journal/view.php?id=" + modul,
	}, nil
}

// h5pIntegrationMarker steht direkt vor dem H5PIntegration-JSON-Objekt im
// Seiten-HTML (gültiges JSON, per json.Decoder direkt aus dem Fließtext lesbar
// — der Decoder stoppt automatisch nach dem einen Objekt, Rest wird ignoriert).
const h5pIntegrationMarker = "var H5PIntegration = "

type h5pIntegration struct {
	PostUserStatistics bool `json:"postUserStatistics"`
	Ajax               struct {
		SetFinished string `json:"setFinished"`
	} `json:"ajax"`
	Contents map[string]struct {
		Library     string `json:"library"`
		JSONContent string `json:"jsonContent"`
	} `json:"contents"`
}

// dragQuestionContent bildet nur die für die Musterlösung nötigen Felder von
// H5P.DragQuestion ab: jedes Element gehört in genau eine Dropzone, die
// Summe der correctElements über alle Dropzones ergibt die maximale
// Punktzahl (1 Punkt je richtig platziertem Element).
type dragQuestionContent struct {
	Question struct {
		Task struct {
			Elements  []any `json:"elements"`
			DropZones []struct {
				CorrectElements []string `json:"correctElements"`
			} `json:"dropZones"`
		} `json:"task"`
	} `json:"question"`
}

// h5pAbschliessen sendet für eine H5P.DragQuestion-Zuordnungsaufgabe eine
// perfekte Musterlösung an setFinished — Moodle verlangt für die
// Abschlussvoraussetzung "Eine Bewertung erhalten" nur Score/MaxScore,
// nicht die tatsächlichen Drag&Drop-Interaktionen im Browser.
func (p *Plugin) h5pAbschliessen(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	modul := strings.TrimSpace(firstNonEmpty(args["modul"], args["id"], args["cmid"]))
	if modul == "" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			`missing required param "modul" (Kursmodul-ID der H5P-Aktivität, siehe inhalt)`)
	}
	if !numRe.MatchString(modul) {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			fmt.Sprintf("ungültige modul-id %q (Zahl aus inhalt erwartet)", modul))
	}

	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}

	viewURL := moodleBase + "/mod/hvp/view.php?id=" + modul
	_, page, err := p.getPage(ctx, viewURL)
	if err != nil {
		return nil, err
	}
	if strings.Contains(page, `id="kc-form-login"`) {
		return nil, coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session abgelaufen (erneut anmelden)")
	}
	idx := strings.Index(page, h5pIntegrationMarker)
	if idx < 0 {
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			"keine H5P-Daten gefunden (modul-id prüfen — ist es eine H5P-Aktivität?)")
	}
	var integ h5pIntegration
	if err := json.NewDecoder(strings.NewReader(page[idx+len(h5pIntegrationMarker):])).Decode(&integ); err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "H5P-Daten unverständlich", err)
	}
	if !integ.PostUserStatistics || integ.Ajax.SetFinished == "" {
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "Bewertungsübermittlung ist für diese Aktivität deaktiviert")
	}

	var cid, library, jsonContent string
	found := []string{}
	for key, ct := range integ.Contents {
		found = append(found, ct.Library)
		if strings.HasPrefix(ct.Library, "H5P.DragQuestion") {
			cid = strings.TrimPrefix(key, "cid-")
			library = ct.Library
			jsonContent = ct.JSONContent
		}
	}
	if cid == "" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			fmt.Sprintf("nicht unterstützter H5P-Typ (gefunden: %s) — nur H5P.DragQuestion (Zuordnungsaufgaben) wird unterstützt", strings.Join(found, ", ")))
	}

	var dq dragQuestionContent
	if err := json.Unmarshal([]byte(jsonContent), &dq); err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "DragQuestion-Inhalt unverständlich", err)
	}
	maxScore := len(dq.Question.Task.Elements)
	if maxScore == 0 {
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "keine Elemente in der Zuordnungsaufgabe gefunden")
	}
	score := 0
	for _, z := range dq.Question.Task.DropZones {
		score += len(z.CorrectElements)
	}
	if score > maxScore {
		score = maxScore
	}

	now := time.Now()
	opened := now.Add(-45 * time.Second)
	form := url.Values{
		"contentId": {cid},
		"score":     {fmt.Sprint(score)},
		"maxScore":  {fmt.Sprint(maxScore)},
		"opened":    {fmt.Sprint(opened.Unix())},
		"finished":  {fmt.Sprint(now.Unix())},
		"time":      {"45"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, integ.Ajax.SetFinished, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := p.httpClient(ctx).Do(req)
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "lernplattform nicht erreichbar", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("lernplattform antwortet mit HTTP %d beim Speichern der Bewertung", resp.StatusCode))
	}
	return map[string]any{
		"modul": modul, "library": library, "score": score, "maxScore": maxScore,
		"antwort": strings.TrimSpace(string(raw)),
	}, nil
}

// extractHiddenFields liest alle <input name="…" value="…">-Paare aus einem
// Formular-Body ein (Attribut-Reihenfolge je Feld ignoriert), Submit/Button
// ausgenommen — die werden gezielt über submitButtonValue gelesen.
func extractHiddenFields(formBody string) map[string]string {
	out := map[string]string{}
	for _, tag := range journalInputTagRe.FindAllString(formBody, -1) {
		attrs := map[string]string{}
		for _, am := range journalAttrRe.FindAllStringSubmatch(tag, -1) {
			attrs[am[1]] = html.UnescapeString(am[2])
		}
		name := attrs["name"]
		if name == "" {
			continue
		}
		typ := strings.ToLower(attrs["type"])
		if typ == "submit" || typ == "button" {
			continue
		}
		out[name] = attrs["value"]
	}
	return out
}

// submitButtonValue liest den (lokalisierten) Beschriftungstext eines
// benannten Submit-Buttons aus dem Formular.
func submitButtonValue(formBody, name string) string {
	for _, tag := range journalInputTagRe.FindAllString(formBody, -1) {
		attrs := map[string]string{}
		for _, am := range journalAttrRe.FindAllStringSubmatch(tag, -1) {
			attrs[am[1]] = html.UnescapeString(am[2])
		}
		if attrs["name"] == name {
			return attrs["value"]
		}
	}
	return ""
}

// wrapParagraphs baut aus reinem Text (kein HTML) Absätze für FORMAT_HTML —
// sonst zeigt Moodle den Text ungebrochen in einer Zeile.
func wrapParagraphs(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	parts := strings.Split(text, "\n\n")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		part = strings.ReplaceAll(html.EscapeString(part), "\n", "<br>")
		out = append(out, "<p>"+part+"</p>")
	}
	if len(out) == 0 {
		return "<p></p>"
	}
	return strings.Join(out, "")
}

// --- Kurs-Status (get_state) ---

type courseState struct {
	Course struct {
		ID string `json:"id"`
	} `json:"course"`
	Section []stateSection `json:"section"`
	CM      []stateCM      `json:"cm"`
}

type stateSection struct {
	ID      string   `json:"id"`
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	CMList  []string `json:"cmlist"`
	Visible bool     `json:"visible"`
}

type stateCM struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Modname string `json:"modname"`
	Module  string `json:"module"`
	URL     string `json:"url"`
	Visible bool   `json:"visible"`
	Section int    `json:"sectionnumber"`
}

// loadCourseState ruft core_courseformat_get_state auf (data ist doppelt
// JSON-codiert) und parst Abschnitte + Module.
func (p *Plugin) loadCourseState(ctx context.Context, course string) (*courseState, error) {
	var raw string
	if err := p.ajax(ctx, "core_courseformat_get_state", map[string]any{"courseid": courseID(course)}, &raw); err != nil {
		return nil, err
	}
	var state courseState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "kursinhalt unverständlich", err)
	}
	return &state, nil
}

// sectionTitle normalisiert leere Titel (Abschnitt 0 = Allgemeines).
func sectionTitle(s stateSection) string {
	if s.Title == "" && s.Number == 0 {
		return "Allgemeines"
	}
	return s.Title
}

// moduleViewURL liefert die Modul-URL; fehlt sie im Status (z.B. Datei/
// Verzeichnis-Module), wird die kanonische mod/view-URL synthetisiert.
func moduleViewURL(m stateCM) string {
	if m.URL != "" {
		return m.URL
	}
	if m.Module != "" && m.ID != "" {
		return moodleBase + "/mod/" + m.Module + "/view.php?id=" + m.ID
	}
	return ""
}

// courseID reicht die Kurs-ID als Zahl oder String an Moodle weiter.
func courseID(s string) any {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err == nil {
		return n
	}
	return s
}

// --- Datei-Download ---

type fileLink struct {
	Name string
	URL  string
}

// fileLinks sammelt pluginfile-Datei-Links einer Modulseite (absolut,
// dedupliziert, mit bereinigtem Link-Text als Dateiname).
func fileLinks(base, page string) []fileLink {
	out := []fileLink{}
	seen := map[string]bool{}
	for _, m := range pluginfileLinkRe.FindAllStringSubmatch(page, -1) {
		u := absolutize(base, html.UnescapeString(m[1]))
		if seen[u] {
			continue
		}
		seen[u] = true
		name := cleanText(m[2])
		if name == "" {
			name = filenameFromURL(u)
		}
		out = append(out, fileLink{Name: name, URL: u})
	}
	return out
}

func cleanText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return wsRe.ReplaceAllString(strings.TrimSpace(s), " ")
}

func absolutize(base, href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if u.IsAbs() {
		return u.String()
	}
	b, err := url.Parse(base)
	if err != nil {
		return href
	}
	return b.ResolveReference(u).String()
}

// cmidFromURL liest ?id= aus einer mod/view-URL (für die Antwort-Deko).
func cmidFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if id := u.Query().Get("id"); numRe.MatchString(id) {
		return id
	}
	return ""
}

func modulOrURL(modul, viewURL string) string {
	if modul != "" {
		return modul
	}
	return viewURL
}

func isHTML(ctype string, body []byte) bool {
	ct := strings.ToLower(ctype)
	if strings.Contains(ct, "text/html") {
		return true
	}
	if ct != "" && !strings.Contains(ct, "text/") && !strings.Contains(ct, "html") {
		return false
	}
	// Ohne Content-Type: Sniffing auf <html/doctype.
	snip := strings.ToLower(string(bytes.TrimSpace(body)))
	if len(snip) > 200 {
		snip = snip[:200]
	}
	return strings.Contains(snip, "<html") || strings.Contains(snip, "<!doctype html")
}

// getBinary lädt eine URL mit Session-Cookies (folgt Redirects) und liefert
// finale URL, Content-Type, Content-Disposition und Bytes (limitiert).
func (p *Plugin) getBinary(ctx context.Context, rawURL string) (finalURL, ctype, disp string, body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", "", nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header)
	resp, err := p.httpClient(ctx).Do(req)
	if err != nil {
		return "", "", "", nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "lernplattform nicht erreichbar", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// ok
	case http.StatusForbidden, http.StatusUnauthorized:
		return "", "", "", nil, coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session abgelaufen (erneut anmelden)")
	case http.StatusNotFound:
		return "", "", "", nil, coreerrors.New(coreerrors.CodeNotFound, p.ID(), "modul nicht gefunden (id/typ prüfen)")
	default:
		return "", "", "", nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("lernplattform antwortet mit HTTP %d", resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return "", "", "", nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort lesen fehlgeschlagen", err)
	}
	if int64(len(raw)) > maxDownload {
		return "", "", "", nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "datei zu groß (>256 MB)")
	}
	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return final, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"), raw, nil
}

// saveDownload bestimmt den Dateinamen (Disposition > URL), löst den
// Zielpfad auf und schreibt die Datei.
func (p *Plugin) saveDownload(args map[string]string, modul, typ, srcURL, ctype, disp string, body []byte) (any, error) {
	name := dispositionName(disp)
	if name == "" {
		name = filenameFromURL(srcURL)
	}
	if name == "" {
		name = "modul-" + modul
	}
	name = sanitizeFilename(name)
	ziel := strings.TrimSpace(firstNonEmpty(args["ziel"], args["ausgabe"], args["pfad"], args["output"]))
	pfad, err := resolveZiel(ziel, name)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(pfad, body, 0o644); err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "datei schreiben fehlgeschlagen", err)
	}
	abs, err := filepath.Abs(pfad)
	if err != nil {
		abs = pfad
	}
	return map[string]any{
		"modul": modul, "typ": typ, "url": srcURL,
		"dateiname": name, "pfad": abs, "bytes": len(body), "content_type": ctype,
	}, nil
}

// dispositionName liest filename*=UTF-8”… oder filename="…" aus.
func dispositionName(disp string) string {
	m := dispNameRe.FindStringSubmatch(disp)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		if dec, err := url.QueryUnescape(m[1]); err == nil {
			return dec
		}
		return m[1]
	}
	return m[2]
}

// filenameFromURL nimmt das letzte Pfadsegment (ohne Query).
func filenameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	seg := strings.TrimSpace(filepath.Base(u.Path))
	if seg == "" || seg == "." || seg == "/" {
		return ""
	}
	if dec, err := url.PathUnescape(seg); err == nil {
		seg = dec
	}
	return seg
}

// sanitizeFilename entfernt Pfadanteile (kein Directory-Traversal).
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.TrimSpace(strings.ReplaceAll(name, "\x00", ""))
	if name == "" || name == "." || name == ".." {
		return "datei"
	}
	return name
}

// resolveZiel: "" → CWD/Name; Ordner → Ordner/Name; sonst Pfad als Datei.
// Elternverzeichnisse werden angelegt.
func resolveZiel(ziel, name string) (string, error) {
	if ziel == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return name, nil
		}
		return filepath.Join(cwd, name), nil
	}
	if strings.HasSuffix(ziel, "/") {
		if err := os.MkdirAll(ziel, 0o755); err != nil {
			return "", coreerrors.Wrap(coreerrors.CodeInternal, "mebis", "zielordner anlegen fehlgeschlagen", err)
		}
		return filepath.Join(ziel, name), nil
	}
	if fi, err := os.Stat(ziel); err == nil && fi.IsDir() {
		return filepath.Join(ziel, name), nil
	}
	if dir := filepath.Dir(ziel); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", coreerrors.Wrap(coreerrors.CodeInternal, "mebis", "zielordner anlegen fehlgeschlagen", err)
		}
	}
	return ziel, nil
}

// --- Auth-Mechanik ---

// creds sind die aufgelösten Login-Daten: ByCS-Kennung + Passwort.
type creds struct {
	Username string
	Password string
}

func (c creds) validate() error {
	if c.Username == "" || c.Password == "" {
		return fmt.Errorf(`login braucht username + password — einmal "auth mebis" aufrufen oder als Parameter mitgeben`)
	}
	return nil
}

// normalizeCreds normalisiert gemergte Credentials (Runtime liefert Store +
// Env + Args bereits zusammengeführt, Aliase normalisiert).
func normalizeCreds(args map[string]string) creds {
	return creds{
		Username: strings.TrimSpace(args["username"]),
		Password: args["password"],
	}
}

// envCredentials löst MEBIS_SECRET auf ("username:passwort"
// oder JSON {"username","password"}). Leere Felder bleiben leer —
// Store/Args füllt die Runtime.
func envCredentials() map[string]string {
	secret := strings.TrimSpace(os.Getenv("MEBIS_SECRET"))
	if secret == "" {
		return nil
	}
	out := map[string]string{}
	if strings.HasPrefix(secret, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(secret), &m) == nil {
			if v := strings.TrimSpace(firstNonEmpty(m["username"], m["benutzername"], m["kennung"])); v != "" {
				out["username"] = v
			}
			if v := firstNonEmpty(m["password"], m["passwort"]); v != "" {
				out["password"] = v
			}
		}
		return out
	}
	if i := strings.Index(secret, ":"); i > 0 {
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

// httpClient liefert den Tenant-eigenen Client mit Cookie-Jar.
func (p *Plugin) httpClient(ctx context.Context) *http.Client {
	return p.sessionFor(ctx).client
}

// ensureAuth meldet an, falls keine Session dieses Tenants für diesen
// Benutzer besteht.
func (p *Plugin) ensureAuth(ctx context.Context, c creds) error {
	s := p.sessionFor(ctx)
	p.mu.Lock()
	ok := s.authed && strings.EqualFold(s.user, c.Username)
	if ok {
		s.secret = c.Password // Passwortwechsel übernehmen, Session bleibt gültig
	}
	p.mu.Unlock()
	if ok {
		return nil
	}
	return p.login(ctx, c)
}

// login fährt den HAR-rekonstruierten Flow:
// GET / (folgt SSO-Redirects bis zur Keycloak-Form) → Form-Action parsen →
// GET / (folgt SSO-Redirects bis zur Keycloak-Form) → Form-Action parsen →
// POST {username, password, credentialId:""} → Redirects bis Moodle folgen →
// sesskey aus Seiten-HTML lesen.
func (p *Plugin) login(ctx context.Context, c creds) error {
	s := p.sessionFor(ctx)
	p.mu.Lock()

	// Frischer Jar pro Login (alte Sessions dieses Tenants verwerfen).
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: p.baseTimeout, Transport: p.baseTransport, Jar: jar}
	p.mu.Unlock()

	// 1. SSO-Kette bis zur Keycloak-Loginseite (Client folgt Redirects).
	_, page, err := getPage(client, ctx, moodleBase+"/", p.ID())
	if err != nil {
		return err
	}
	m := formRe.FindStringSubmatch(page)
	if m == nil {
		return coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			"keycloak-loginseite nicht gefunden (SSO-Kette unerwartet — Seite hat sich geändert?)")
	}
	action := html.UnescapeString(m[1])

	// 2. Credentials posten (keinen Redirects folgen — Location auswerten).
	form := url.Values{}
	form.Set("username", c.Username)
	form.Set("password", c.Password)
	form.Set("credentialId", "")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, action, strings.NewReader(form.Encode()))
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noRedirect := *client
	noRedirect.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "lernplattform nicht erreichbar", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if (resp.StatusCode == http.StatusUnauthorized) ||
		(resp.StatusCode == http.StatusOK && strings.Contains(string(raw), `id="kc-form-login"`)) {
		return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(),
			"login abgelehnt (username/passwort prüfen)")
	}
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		return coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("login antwortet unerwartet mit HTTP %d (Formulardetails prüfen)", resp.StatusCode))
	}

	// 3. Callback-Redirects bis Moodle folgen, sesskey auslesen.
	finalURL, page, err := followRedirects(client, ctx, loc, p.ID())
	if err != nil {
		return err
	}
	_ = finalURL
	sm := sesskeyRe.FindStringSubmatch(page)
	if sm == nil {
		return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(),
			"session-check nach login fehlgeschlagen (kein sesskey — ggf. Zugangsdaten falsch)")
	}

	p.mu.Lock()
	s.client = client
	s.authed = true
	s.user = c.Username
	s.secret = c.Password
	s.sesskey = sm[1]
	p.mu.Unlock()
	return nil
}

// getPage lädt eine Seite (folgt Redirects) und liefert finale URL + Body.
func (p *Plugin) getPage(ctx context.Context, rawURL string) (string, string, error) {
	return getPage(p.httpClient(ctx), ctx, rawURL, p.ID())
}

// getPage lädt eine Seite mit explizitem Client (folgt Redirects).
func getPage(client *http.Client, ctx context.Context, rawURL, pluginID string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", coreerrors.Wrap(coreerrors.CodeInternal, pluginID, "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", coreerrors.Wrap(coreerrors.CodeUpstream, pluginID, "lernplattform nicht erreichbar", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return "", "", coreerrors.Wrap(coreerrors.CodeUpstream, pluginID, "antwort lesen fehlgeschlagen", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", coreerrors.New(coreerrors.CodeUpstream, pluginID,
			fmt.Sprintf("lernplattform antwortet mit HTTP %d", resp.StatusCode))
	}
	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return final, string(raw), nil
}

// followRedirects folgt Location-Redirects manuell (für die Login-Kette),
// damit Cookies pro Hop im Jar landen, und liefert finale URL + Body.
func (p *Plugin) followRedirects(ctx context.Context, loc string) (string, string, error) {
	return followRedirects(p.httpClient(ctx), ctx, loc, p.ID())
}

func followRedirects(client *http.Client, ctx context.Context, loc, pluginID string) (string, string, error) {
	current := loc
	for hops := 0; hops < 10; hops++ {
		u, err := url.Parse(current)
		if err != nil {
			return "", "", coreerrors.Wrap(coreerrors.CodeUpstream, pluginID, "redirect-url ungültig", err)
		}
		if !u.IsAbs() {
			base, _ := url.Parse(moodleBase + "/")
			u = base.ResolveReference(u)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return "", "", coreerrors.Wrap(coreerrors.CodeInternal, pluginID, "request bauen fehlgeschlagen", err)
		}
		setBrowserHeaders(req.Header)
		noRedirect := *client
		noRedirect.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := noRedirect.Do(req)
		if err != nil {
			return "", "", coreerrors.Wrap(coreerrors.CodeUpstream, pluginID, "lernplattform nicht erreichbar", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
		resp.Body.Close()
		next := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode > 399 || next == "" {
			if resp.StatusCode != http.StatusOK {
				return "", "", coreerrors.New(coreerrors.CodeUpstream, pluginID,
					fmt.Sprintf("login-kette endet mit HTTP %d", resp.StatusCode))
			}
			return u.String(), string(raw), nil
		}
		nu, err := url.Parse(next)
		if err != nil {
			return "", "", coreerrors.Wrap(coreerrors.CodeUpstream, pluginID, "redirect-url ungültig", err)
		}
		current = u.ResolveReference(nu).String()
	}
	return "", "", coreerrors.New(coreerrors.CodeUpstream, pluginID, "zu viele redirects in der login-kette")
}

// ajax ruft eine Moodle-Webservice-Funktion auf. Bei abgelaufener Session
// (sesskey-Fehler / Loginseite statt JSON) wird mit den gespeicherten
// Session-Credentials des Tenants einmal neu angemeldet und wiederholt.
func (p *Plugin) ajax(ctx context.Context, method string, args map[string]any, out any) error {
	if err := p.doAjax(ctx, method, args, out); err != nil {
		if ce, ok := err.(*coreerrors.Error); ok && ce.Code == coreerrors.CodeUnauthorized {
			s := p.sessionFor(ctx)
			p.mu.Lock()
			s.authed = false
			c := creds{Username: s.user, Password: s.secret}
			p.mu.Unlock()
			if c.validate() == nil {
				if lerr := p.login(ctx, c); lerr == nil {
					return p.doAjax(ctx, method, args, out)
				}
			}
		}
		return err
	}
	return nil
}

func (p *Plugin) doAjax(ctx context.Context, method string, args map[string]any, out any) error {
	s := p.sessionFor(ctx)
	p.mu.Lock()
	sesskey := s.sesskey
	client := s.client
	p.mu.Unlock()
	if sesskey == "" {
		return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "keine session (erneut anmelden)")
	}
	payload, _ := json.Marshal([]map[string]any{{"index": 0, "methodname": method, "args": args}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		moodleBase+"/lib/ajax/service.php?sesskey="+url.QueryEscape(sesskey)+"&info="+url.QueryEscape(method),
		bytes.NewReader(payload))
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	setBrowserHeaders(req.Header)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "lernplattform nicht erreichbar", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort lesen fehlgeschlagen", err)
	}
	if resp.StatusCode == http.StatusForbidden {
		return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session abgelaufen (erneut anmelden)")
	}
	if resp.StatusCode != http.StatusOK {
		return coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("lernplattform antwortet mit HTTP %d", resp.StatusCode))
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		// Keine JSON-Envelope → typischerweise Loginseite (Session tot).
		return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session abgelaufen (erneut anmelden)")
	}
	var envelope []struct {
		Error bool            `json:"error"`
		Data  json.RawMessage `json:"data"`
		Exc   *struct {
			Message string `json:"message"`
		} `json:"exception"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope) == 0 {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort unverständlich", err)
	}
	el := envelope[0]
	if el.Error {
		msg := "webservice meldet fehler"
		if el.Exc != nil && el.Exc.Message != "" {
			msg += ": " + el.Exc.Message
		}
		if strings.Contains(strings.ToLower(msg), "sesskey") ||
			strings.Contains(strings.ToLower(msg), "session") ||
			strings.Contains(strings.ToLower(msg), "login") {
			return coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session abgelaufen (erneut anmelden)")
		}
		return coreerrors.New(coreerrors.CodeUpstream, p.ID(), msg)
	}
	if err := json.Unmarshal(el.Data, out); err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort unverständlich", err)
	}
	return nil
}

// setBrowserHeaders setzt die Header, die Moodle laut HAR mitschickt.
func setBrowserHeaders(h http.Header) {
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/json,text/plain,*/*;q=0.8")
	h.Set("User-Agent", "SchoolConnect/1.0")
}
