// Package bycsdrive kapselt ByCS Drive (ownCloud OCIS hinter Keycloak,
// z.B. https://0978.drive.bycs.de).
//
// Login (aus HAR-Mitschnitt rekonstruiert, Token-Endpunkt live verifiziert):
//  1. GET https://<host>/config.json → openIdConnect {authority, client_id}
//     (z.B. authority https://auth.drive.bycs.de/realms/bycs,
//     client_id school-0978-web).
//  2. POST {authority}/protocol/openid-connect/token mit
//     application/x-www-form-urlencoded
//     {grant_type=password, client_id, username, password,
//     scope="openid profile email"} → {access_token (5 Min),
//     refresh_token, expires_in}.
//  3. Alle Daten-Calls mit Header "Authorization: Bearer <access_token>":
//     Spaces via Graph (GET /graph/v1beta1/me/drives?$filter=driveType eq …),
//     Ordner via WebDAV (PROPFIND /dav/spaces/<space-id>/<pfad>, Depth: 1),
//     Download via GET /dav/spaces/<space-id>/<pfad>.
//     Bei 401 wird einmal über den Refresh-Token (sonst das Passwort) erneuert
//     und wiederholt.
//
// Auth gehört der Runtime (siehe domain.Plugin.AuthParams): Das Plugin
// deklariert nur host/username/password, die Runtime generiert daraus
// "auth"/"logout", speichert erfolgreiche Logins im Session-Store und
// injiziert die Credentials bei jedem Call. Die Datenfunktionen brauchen
// daher keine Credential-Params — egal ob CLI, REST oder MCP aufruft.
package bycsdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	coreerrors "schoolconnect/internal/core/errors"
	"schoolconnect/internal/domain"
)

const (
	defaultHost = "0978.drive.bycs.de"
	maxBodyRead = 8 << 20
	maxDownload = 512 << 20
)

// propfindBody fragt genau die Properties ab, die auch das Web-UI per
// PROPFIND anfordert (Depth: 1).
const propfindBody = `<d:propfind xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">` +
	`<d:prop><oc:permissions/><oc:favorite/><oc:fileid/><oc:file-parent/><oc:name/>` +
	`<d:lockdiscovery/><d:activelock/><oc:owner-id/><oc:owner-display-name/>` +
	`<oc:remote-item-id/><oc:shareroot/><oc:share-types/><oc:privatelink/>` +
	`<d:getcontentlength/><oc:size/><d:getlastmodified/><d:getetag/>` +
	`<d:getcontenttype/><d:resourcetype/><oc:downloadURL/><oc:tags/>` +
	`<oc:audio/><oc:location/><oc:image/><oc:photo/>` +
	`</d:prop></d:propfind>`

var (
	hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	numRe  = regexp.MustCompile(`^[0-9]+$`)
)

// dispNameRe liest den Dateinamen aus Content-Disposition.
var dispNameRe = regexp.MustCompile(`(?i)filename\*=UTF-8''([^;]+)|filename="([^"]+)"`)

// Plugin implementiert domain.Plugin.
type Plugin struct {
	client *http.Client // eigener Client (Bearer-Auth, keine Cookies nötig)

	mu       sync.Mutex
	authed   bool
	host     string // Drive-Instanz, für die das Token gilt
	user     string // ByCS-Kennung, für die das Token gilt
	secret   string // Passwort (nur in-memory, für Re-Login)
	access   string // OIDC-Access-Token (kurzlebig, ~5 Min)
	refresh  string // OIDC-Refresh-Token
	expiry   time.Time
	tokenURL string // Token-Endpunkt der Instanz
	clientID string // OIDC-Client der Instanz
}

// New erzeugt das Plugin. Der Core-Client liefert Timeout/Transport.
func New(client *http.Client) *Plugin {
	timeout := 15 * time.Second
	var transport http.RoundTripper
	if client != nil {
		if client.Timeout > 0 {
			timeout = client.Timeout
		}
		transport = client.Transport
	}
	return &Plugin{client: &http.Client{Timeout: timeout, Transport: transport}}
}

func (p *Plugin) ID() string   { return "bycs-drive" }
func (p *Plugin) Name() string { return "ByCS Drive" }
func (p *Plugin) Description() string {
	return "ByCS Drive (Dateicloud): Login, Spaces, Ordner und Datei-Download."
}

// AuthParams deklariert die Login-Credentials für die Runtime.
// Daraus generiert die Runtime automatisch "auth"/"logout".
func (p *Plugin) AuthParams() []domain.Param {
	return []domain.Param{
		{Name: "host", Description: "Drive-Instanz, z.B. 0978.drive.bycs.de (0978 reicht).", Default: defaultHost, Aliases: []string{"instanz", "schule"}},
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

// EnvCredentials steuert Env-Fallback bei (BYCS_DRIVE_SECRET:
// "host:username:passwort", legacy "username:passwort" oder JSON
// {"host","username","password"}).
func (p *Plugin) EnvCredentials() map[string]string {
	return envCredentials()
}

// --- Functions ---
// Hinweis: keine Credential-Params an Datenfunktionen — die injiziert
// die Runtime aus dem Session-Store (einmal auth, überall angemeldet).

func (p *Plugin) Functions() []domain.Function {
	return []domain.Function{
		{
			Name:        "spaces",
			Description: "Alle eigenen Spaces (Ablagen) auflisten: persönlicher Speicher + geteilte Projekt-Spaces.",
			Params: []domain.Param{
				{Name: "query", Description: "Optional filtern nach Space-Name/Alias (Teiltreffer, case-insensitiv)."},
			},
			Handler: p.spaces,
		},
		{
			Name:        "list",
			Description: "Ordner in einem Space auflisten (Dateien + Unterordner, eine Ebene). Einstieg: spaces, dann pfad absteigen.",
			Params: []domain.Param{
				{Name: "space", Description: "Space-ID oder Name-Teiltreffer aus spaces (z.B. 1k1_wi).", Required: true},
				{Name: "pfad", Description: "Ordnerpfad im Space, z.B. /2.Halbjahr. Default: Wurzel.", Default: "/", Aliases: []string{"path", "ordner"}},
				{Name: "query", Description: "Optional filtern nach Dateiname (Teiltreffer, case-insensitiv)."},
			},
			Handler: p.list,
		},
		{
			Name:        "fetch",
			Description: "Datei aus einem Space laden und lokal speichern.",
			Params: []domain.Param{
				{Name: "space", Description: "Space-ID oder Name-Teiltreffer aus spaces.", Aliases: []string{"drive"}},
				{Name: "pfad", Description: "Dateipfad im Space, z.B. /2.Halbjahr/Blatt.pdf. Alternativ url angeben.", Aliases: []string{"path", "datei", "name"}},
				{Name: "url", Description: "Volle DAV-URL als Alternative (…/dav/spaces/<space-id>/<pfad>)."},
				{Name: "ziel", Description: "Zielpfad (Datei oder Ordner). Default: aktueller Ordner + Dateiname.", Aliases: []string{"ausgabe", "pfad_ziel", "output"}},
			},
			Handler: p.fetch,
		},
	}
}

// --- Handler ---

func (p *Plugin) spaces(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	drives, err := p.loadDrives(ctx, c)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(args["query"]))
	out := []any{}
	for _, d := range drives {
		if q != "" && !strings.Contains(strings.ToLower(d.Name), q) &&
			!strings.Contains(strings.ToLower(d.Alias), q) {
			continue
		}
		out = append(out, map[string]any{
			"space": d.ID, "name": d.Name, "typ": d.Type, "alias": d.Alias,
			"quota_used": d.QuotaUsed, "quota_total": d.QuotaTotal,
			"quota_remaining": d.QuotaRemaining, "url": d.WebURL,
		})
	}
	return map[string]any{"angemeldet_als": c.Username, "count": len(out), "spaces": out}, nil
}

func (p *Plugin) list(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	spaceQ := strings.TrimSpace(args["space"])
	if spaceQ == "" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), `missing required param "space" (siehe spaces)`)
	}
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	space, err := p.resolveSpace(ctx, c, spaceQ)
	if err != nil {
		return nil, err
	}
	pfad := cleanRel(firstNonEmpty(args["pfad"], args["path"], args["ordner"]))
	entries, err := p.propfind(ctx, c, space, pfad)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(args["query"]))
	out := []any{}
	for _, e := range entries {
		if q != "" && !strings.Contains(strings.ToLower(e.Name), q) {
			continue
		}
		out = append(out, map[string]any{
			"name": e.Name, "pfad": e.Path, "typ": e.Type,
			"groesse": e.Size, "geaendert": e.Modified, "content_type": e.ContentType,
		})
	}
	anzeige := "/"
	if pfad != "" {
		anzeige = "/" + pfad
	}
	return map[string]any{
		"space": space.ID, "space_name": space.Name, "pfad": anzeige,
		"count": len(out), "eintraege": out,
	}, nil
}

func (p *Plugin) fetch(ctx context.Context, args map[string]string) (any, error) {
	c := normalizeCreds(args)
	if err := c.validate(); err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	rawURL := strings.TrimSpace(args["url"])
	pfad := cleanRel(firstNonEmpty(args["pfad"], args["path"], args["datei"], args["name"]))
	spaceQ := strings.TrimSpace(firstNonEmpty(args["space"], args["drive"]))
	var davURL, spaceID, spaceName string
	if rawURL == "" {
		if spaceQ == "" {
			return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
				`entweder "space" + "pfad" (siehe spaces/list) oder "url" angeben`)
		}
		if pfad == "" {
			return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
				`"pfad" fehlt (Dateipfad im Space, siehe list)`)
		}
		if err := p.ensureAuth(ctx, c); err != nil {
			return nil, err
		}
		space, err := p.resolveSpace(ctx, c, spaceQ)
		if err != nil {
			return nil, err
		}
		davURL = davURLFor(normalizeHost(c.Host), space.ID, pfad)
		spaceID, spaceName = space.ID, space.Name
	} else {
		u, err := url.Parse(rawURL)
		if err != nil || !strings.HasPrefix(u.Path, "/dav/spaces/") {
			return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
				`ungültige url (DAV-URL wie …/dav/spaces/<space-id>/<pfad> erwartet)`)
		}
		if err := p.ensureAuth(ctx, c); err != nil {
			return nil, err
		}
		davURL = rawURL
		rest := strings.TrimPrefix(u.Path, "/dav/spaces/")
		if i := strings.Index(rest, "/"); i >= 0 {
			spaceID, pfad = rest[:i], strings.Trim(rest[i+1:], "/")
		} else {
			spaceID = rest
		}
	}

	final, ctype, disp, body, err := p.getBinary(ctx, c, davURL)
	if err != nil {
		return nil, err
	}
	if isDirBody(ctype, body) {
		hinweis := "pfad ist ein Ordner — mit list den Inhalt anzeigen"
		if pfad != "" {
			hinweis = fmt.Sprintf("pfad ist ein Ordner — mit list (pfad %q) den Inhalt anzeigen", "/"+pfad)
		}
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), hinweis)
	}
	if isHTML(ctype, body) {
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "antwort ist kein Datei-Download (Loginseite/Fehlerseite?)")
	}
	return p.saveDownload(args, spaceID, spaceName, pfad, final, ctype, disp, body)
}

// --- Graph: Spaces ---

type drive struct {
	ID             string
	Name           string
	Type           string
	Alias          string
	QuotaUsed      int64
	QuotaTotal     int64
	QuotaRemaining int64
	WebURL         string
}

// loadDrives lädt persönliche + Projekt-Spaces (zwei gefilterte Calls wie
// das Web-UI) und gibt sie in stabiler Reihenfolge zurück.
func (p *Plugin) loadDrives(ctx context.Context, c creds) ([]drive, error) {
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	out := []drive{}
	for _, typ := range []string{"personal", "project"} {
		drives, err := p.listDrives(ctx, c, typ)
		if err != nil {
			return nil, err
		}
		out = append(out, drives...)
	}
	return out, nil
}

func (p *Plugin) listDrives(ctx context.Context, c creds, driveType string) ([]drive, error) {
	if err := p.ensureAuth(ctx, c); err != nil {
		return nil, err
	}
	host := normalizeHost(c.Host)
	q := url.Values{}
	q.Set("$orderby", "name asc")
	q.Set("$filter", "driveType eq "+driveType)
	rawURL := "https://" + host + "/graph/v1beta1/me/drives?" + q.Encode()
	var data struct {
		Value []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Type  string `json:"driveType"`
			Alias string `json:"driveAlias"`
			Quota struct {
				Used      int64 `json:"used"`
				Total     int64 `json:"total"`
				Remaining int64 `json:"remaining"`
			} `json:"quota"`
			WebURL string `json:"webUrl"`
		} `json:"value"`
	}
	if err := p.graphGet(ctx, c, rawURL, &data); err != nil {
		return nil, err
	}
	out := []drive{}
	for _, d := range data.Value {
		out = append(out, drive{
			ID: d.ID, Name: d.Name, Type: d.Type, Alias: d.Alias,
			QuotaUsed: d.Quota.Used, QuotaTotal: d.Quota.Total,
			QuotaRemaining: d.Quota.Remaining, WebURL: d.WebURL,
		})
	}
	return out, nil
}

// resolveSpace löst Space-ID oder Name-/Alias-Teiltreffer auf die volle
// Space-ID auf. Bei Mehrdeutigkeit wird abgebrochen statt zu raten.
func (p *Plugin) resolveSpace(ctx context.Context, c creds, query string) (drive, error) {
	drives, err := p.loadDrives(ctx, c)
	if err != nil {
		return drive{}, err
	}
	for _, d := range drives {
		if d.ID == query {
			return d, nil
		}
	}
	q := strings.ToLower(query)
	treffer := []drive{}
	for _, d := range drives {
		if strings.Contains(strings.ToLower(d.Name), q) ||
			strings.Contains(strings.ToLower(d.Alias), q) {
			treffer = append(treffer, d)
		}
	}
	if len(treffer) == 0 {
		return drive{}, coreerrors.New(coreerrors.CodeNotFound, p.ID(),
			fmt.Sprintf("space %q nicht gefunden (spaces listet alle)", query))
	}
	if len(treffer) > 1 {
		namen := []string{}
		for _, d := range treffer {
			namen = append(namen, d.Name+" ("+d.ID+")")
		}
		sort.Strings(namen)
		return drive{}, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			fmt.Sprintf("%q trifft %d spaces — bitte eindeutige space-id angeben: %s", query, len(treffer), strings.Join(namen, ", ")))
	}
	return treffer[0], nil
}

// --- WebDAV: Ordner + Download ---

type davEntry struct {
	Name        string
	Path        string // Anzeigepfad ab Space-Wurzel ("/…")
	Type        string // "ordner" oder "datei"
	Size        int64
	Modified    string
	ContentType string
}

type davMultistatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href      string        `xml:"href"`
	Propstats []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	Name         string `xml:"name"`
	FileID       string `xml:"fileid"`
	Permissions  string `xml:"permissions"`
	OwnerID      string `xml:"owner-id"`
	OwnerName    string `xml:"owner-display-name"`
	Privatelink  string `xml:"privatelink"`
	Size         string `xml:"size"`
	ContentLen   string `xml:"getcontentlength"`
	LastModified string `xml:"getlastmodified"`
	Etag         string `xml:"getetag"`
	ContentType  string `xml:"getcontenttype"`
	ResourceType struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
}

// propfind listet eine Ordnerebene (Depth: 1). Der Ordner selbst (erste
// Response) wird übersprungen; Ordner stehen vor Dateien.
func (p *Plugin) propfind(ctx context.Context, c creds, space drive, rel string) ([]davEntry, error) {
	host := normalizeHost(c.Host)
	rawURL := davURLFor(host, space.ID, rel)
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", rawURL, strings.NewReader(propfindBody))
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", "1")
	var ms davMultistatus
	if err := p.davDo(ctx, c, req, &ms, true); err != nil {
		return nil, err
	}
	dirPrefix := "/dav/spaces/" + space.ID
	reqPath := dirPrefix
	if rel != "" {
		reqPath += "/" + rel
	}
	out := []davEntry{}
	for _, r := range ms.Responses {
		rp := hrefPath(r.Href)
		if rp == "" {
			continue
		}
		if samePath(rp, reqPath) {
			continue // der Ordner selbst
		}
		relPart := strings.TrimPrefix(rp, strings.TrimSuffix(reqPath, "/")+"/")
		if relPart == rp {
			continue // gehört nicht hierher (sollte bei Depth: 1 nicht passieren)
		}
		prop, ok := okProp(r)
		if !ok {
			continue
		}
		name := prop.Name
		if name == "" {
			name = path.Base(strings.TrimSuffix(rp, "/"))
		}
		childRel := strings.Trim(path.Join(rel, relPart), "/")
		typ := "datei"
		if prop.ResourceType.Collection != nil {
			typ = "ordner"
		}
		groesse := parseSize(prop.ContentLen)
		if groesse == 0 {
			groesse = parseSize(prop.Size)
		}
		out = append(out, davEntry{
			Name: name, Path: "/" + childRel, Type: typ,
			Size: groesse, Modified: prop.LastModified, ContentType: prop.ContentType,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type == "ordner"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// getBinary lädt eine Datei (folgt Redirects) und liefert finale URL,
// Content-Type, Content-Disposition und Bytes (limitiert).
func (p *Plugin) getBinary(ctx context.Context, c creds, rawURL string) (finalURL, ctype, disp string, body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", "", nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	var raw []byte
	var resp *http.Response
	if raw, resp, err = p.davDoRaw(ctx, c, req); err != nil {
		return "", "", "", nil, err
	}
	if int64(len(raw)) > maxDownload {
		return "", "", "", nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "datei zu groß (>512 MB)")
	}
	final := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return final, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"), raw, nil
}

// saveDownload bestimmt den Dateinamen (Disposition > URL), löst den
// Zielpfad auf und schreibt die Datei.
func (p *Plugin) saveDownload(args map[string]string, spaceID, spaceName, rel, srcURL, ctype, disp string, body []byte) (any, error) {
	name := dispositionName(disp)
	if name == "" {
		name = filenameFromURL(srcURL)
	}
	if name == "" {
		name = "datei"
	}
	name = sanitizeFilename(name)
	ziel := strings.TrimSpace(firstNonEmpty(args["ziel"], args["ausgabe"], args["pfad_ziel"], args["output"]))
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
	anzeige := "/"
	if rel != "" {
		anzeige = "/" + rel
	}
	return map[string]any{
		"space": spaceID, "space_name": spaceName, "pfad": anzeige, "url": srcURL,
		"dateiname": name, "ziel": abs, "bytes": len(body), "content_type": ctype,
	}, nil
}

// --- HTTP mit Bearer + 401-Retry ---

// graphGet lädt eine Graph-URL (JSON) mit Bearer-Token.
func (p *Plugin) graphGet(ctx context.Context, c creds, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	req.Header.Set("Accept", "application/json")
	body, _, err := p.davDoRaw(ctx, c, req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort unverständlich", err)
	}
	return nil
}

// davDo schickt einen WebDAV-Request und parst 207-Multistatus-XML.
// Bei 401 wird das Token einmal erneuert und wiederholt.
func (p *Plugin) davDo(ctx context.Context, c creds, req *http.Request, out any, want207 bool) error {
	body, resp, err := p.davDoRaw(ctx, c, req)
	if err != nil {
		return err
	}
	if want207 && resp.StatusCode != 207 {
		return coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("drive antwortet mit HTTP %d", resp.StatusCode))
	}
	if err := xml.Unmarshal(body, out); err != nil {
		return coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort unverständlich", err)
	}
	return nil
}

// davDoRaw schickt einen Request mit Bearer-Token. Bei 401 wird das Token
// einmal erneuert (Refresh-Token, sonst Passwort-Login) und wiederholt.
func (p *Plugin) davDoRaw(ctx context.Context, c creds, req *http.Request) ([]byte, *http.Response, error) {
	tok, err := p.bearer(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	setAuth(req, tok)
	body, resp, err := p.do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return body, resp, nil
	}
	// Token tot: einmal erneuern und wiederholen.
	p.markStale()
	tok, err = p.bearer(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	// Body war ein GET ohne Body oder PROPFIND mit String-Body — neu bauen.
	req2 := req.Clone(ctx)
	if req.Body != nil {
		if req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
			}
			req2.Body = b
		} else {
			// PROPFIND-Body ist rekonstruierbar (einzige Methode mit Body).
			req2.Body = io.NopCloser(strings.NewReader(propfindBody))
		}
	}
	setAuth(req2, tok)
	return p.do(req2)
}

// do führt den Request aus und mappt Statuscodes auf Fehler.
func (p *Plugin) do(req *http.Request) ([]byte, *http.Response, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "drive nicht erreichbar", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort lesen fehlgeschlagen", err)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusMultiStatus:
		return raw, resp, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, nil, coreerrors.New(coreerrors.CodeUnauthorized, p.ID(), "session abgelaufen (erneut anmelden)")
	case http.StatusNotFound:
		return nil, nil, coreerrors.New(coreerrors.CodeNotFound, p.ID(), "nicht gefunden (space/pfad prüfen)")
	default:
		return nil, nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("drive antwortet mit HTTP %d", resp.StatusCode))
	}
}

func setAuth(req *http.Request, tok string) {
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", "SchoolConnect/1.0")
}

// --- Auth-Mechanik ---

// creds sind die aufgelösten Login-Daten: Instanz + ByCS-Kennung + Passwort.
type creds struct {
	Host     string
	Username string
	Password string
}

func (c creds) validate() error {
	if c.Username == "" || c.Password == "" {
		return fmt.Errorf(`login braucht username + password — einmal "auth bycs-drive" aufrufen oder als Parameter mitgeben`)
	}
	if normalizeHost(c.Host) == "" {
		return fmt.Errorf(`ungültiger host %q (z.B. 0978.drive.bycs.de)`, c.Host)
	}
	return nil
}

// normalizeCreds normalisiert gemergte Credentials (Runtime liefert Store +
// Env + Args bereits zusammengeführt, Aliase normalisiert).
func normalizeCreds(args map[string]string) creds {
	return creds{
		Host:     strings.TrimSpace(firstNonEmpty(args["host"], args["instanz"], args["schule"])),
		Username: strings.TrimSpace(args["username"]),
		Password: args["password"],
	}
}

// normalizeHost akzeptiert "0978", "0978.drive.bycs.de" und volle URLs.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if strings.Contains(h, "://") {
		if u, err := url.Parse(h); err == nil && u.Host != "" {
			h = u.Host
		}
	}
	h = strings.ToLower(strings.TrimSuffix(h, "/"))
	if numRe.MatchString(h) {
		h += ".drive.bycs.de"
	}
	if !hostRe.MatchString(h) || !strings.Contains(h, ".") {
		return ""
	}
	return h
}

// envCredentials löst BYCS_DRIVE_SECRET auf ("host:username:passwort",
// legacy "username:passwort" oder JSON {"host","username","password"}).
func envCredentials() map[string]string {
	secret := strings.TrimSpace(os.Getenv("BYCS_DRIVE_SECRET"))
	if secret == "" {
		return nil
	}
	out := map[string]string{}
	if strings.HasPrefix(secret, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(secret), &m) == nil {
			if v := strings.TrimSpace(firstNonEmpty(m["host"], m["instanz"], m["schule"])); v != "" {
				out["host"] = v
			}
			if v := strings.TrimSpace(firstNonEmpty(m["username"], m["benutzername"], m["kennung"])); v != "" {
				out["username"] = v
			}
			if v := firstNonEmpty(m["password"], m["passwort"]); v != "" {
				out["password"] = v
			}
		}
		return out
	}
	parts := strings.SplitN(secret, ":", 3)
	if len(parts) == 3 && (numRe.MatchString(strings.TrimSpace(parts[0])) ||
		strings.Contains(parts[0], ".drive.bycs.de")) {
		// host:username:passwort (Passwort darf ":" enthalten)
		if v := strings.TrimSpace(parts[0]); v != "" {
			out["host"] = v
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

// ensureAuth meldet an, falls kein gültiges Token für Benutzer+Instanz besteht.
func (p *Plugin) ensureAuth(ctx context.Context, c creds) error {
	_, err := p.bearer(ctx, c)
	return err
}

// bearer liefert ein gültiges Access-Token (erneuert bei Bedarf).
func (p *Plugin) bearer(ctx context.Context, c creds) (string, error) {
	host := normalizeHost(c.Host)
	p.mu.Lock()
	if p.authed && strings.EqualFold(p.user, c.Username) && p.host == host &&
		p.access != "" && time.Now().Before(p.expiry) {
		tok := p.access
		if c.Password != "" {
			p.secret = c.Password // Passwortwechsel übernehmen
		}
		p.mu.Unlock()
		return tok, nil
	}
	refresh, tokenURL, clientID := "", "", ""
	if p.authed && strings.EqualFold(p.user, c.Username) && p.host == host && p.refresh != "" {
		refresh, tokenURL, clientID = p.refresh, p.tokenURL, p.clientID
	}
	p.mu.Unlock()

	if refresh != "" {
		if err := p.refreshAuth(ctx, host, tokenURL, clientID, refresh); err == nil {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.access, nil
		}
		// Refresh fehlgeschlagen → voller Login mit Passwort.
	}
	if err := p.login(ctx, c); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.access, nil
}

// markStale verwirft das aktuelle Token (nach 401), behält aber Benutzer +
// Refresh-Token für die Erneuerung.
func (p *Plugin) markStale() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.access = ""
	p.expiry = time.Time{}
}

// oidcConfig liest Authority + Client-ID aus /config.json der Instanz.
func (p *Plugin) oidcConfig(ctx context.Context, host string) (tokenURL, clientID string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/config.json", nil)
	if err != nil {
		return "", "", coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "drive nicht erreichbar", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return "", "", coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort lesen fehlgeschlagen", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("drive antwortet mit HTTP %d", resp.StatusCode))
	}
	var cfg struct {
		OpenID struct {
			Authority string `json:"authority"`
			ClientID  string `json:"client_id"`
		} `json:"openIdConnect"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.OpenID.Authority == "" || cfg.OpenID.ClientID == "" {
		return "", "", coreerrors.New(coreerrors.CodeUpstream, p.ID(), "drive-konfiguration unverständlich (config.json)")
	}
	if !strings.HasPrefix(cfg.OpenID.Authority, "https://") {
		return "", "", coreerrors.New(coreerrors.CodeUpstream, p.ID(), "drive-konfiguration ungültig (authority)")
	}
	return strings.TrimSuffix(cfg.OpenID.Authority, "/") + "/protocol/openid-connect/token",
		cfg.OpenID.ClientID, nil
}

// login fährt den Password-Grant: config.json → Token-Endpunkt → Tokens.
func (p *Plugin) login(ctx context.Context, c creds) error {
	host := normalizeHost(c.Host)
	tokenURL, clientID, err := p.oidcConfig(ctx, host)
	if err != nil {
		return err
	}
	tok, err := p.passwordGrant(ctx, tokenURL, clientID, c.Username, c.Password)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authed = true
	p.host = host
	p.user = c.Username
	p.secret = c.Password
	p.access = tok.Access
	p.refresh = tok.Refresh
	p.expiry = time.Now().Add(time.Duration(tok.ExpiresIn-30) * time.Second)
	p.tokenURL = tokenURL
	p.clientID = clientID
	return nil
}

// refreshAuth erneuert das Access-Token über den Refresh-Token.
func (p *Plugin) refreshAuth(ctx context.Context, host, tokenURL, clientID, refresh string) error {
	if tokenURL == "" || clientID == "" {
		var err error
		if tokenURL, clientID, err = p.oidcConfig(ctx, host); err != nil {
			return err
		}
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", refresh)
	tok, err := p.tokenPost(ctx, tokenURL, form)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.access = tok.Access
	if tok.Refresh != "" {
		p.refresh = tok.Refresh
	}
	p.expiry = time.Now().Add(time.Duration(tok.ExpiresIn-30) * time.Second)
	if tokenURL != "" {
		p.tokenURL = tokenURL
	}
	if clientID != "" {
		p.clientID = clientID
	}
	return nil
}

type tokenResp struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ExpiresIn int    `json:"expires_in"`
}

// passwordGrant holt Tokens per Resource-Owner-Password-Flow.
func (p *Plugin) passwordGrant(ctx context.Context, tokenURL, clientID, username, password string) (*tokenResp, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", clientID)
	form.Set("username", username)
	form.Set("password", password)
	form.Set("scope", "openid profile email")
	return p.tokenPost(ctx, tokenURL, form)
}

func (p *Plugin) tokenPost(ctx context.Context, tokenURL string, form url.Values) (*tokenResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "login-server nicht erreichbar", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort lesen fehlgeschlagen", err)
	}
	var data struct {
		tokenResp
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "login-antwort unverständlich", err)
	}
	if data.Error != "" || data.Access == "" {
		if data.Error == "invalid_grant" {
			return nil, coreerrors.New(coreerrors.CodeUnauthorized, p.ID(),
				"login abgelehnt (username/passwort prüfen)")
		}
		msg := data.ErrorDescription
		if msg == "" {
			msg = data.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(), "login fehlgeschlagen: "+msg)
	}
	if data.ExpiresIn <= 60 {
		data.ExpiresIn = 300
	}
	return &data.tokenResp, nil
}

// --- DAV-Helfer ---

// davURLFor baut die WebDAV-URL für Host + Space-ID + relativen Pfad.
func davURLFor(host, spaceID, rel string) string {
	u := &url.URL{Scheme: "https", Host: host, Path: "/dav/spaces/" + spaceID}
	if rel != "" {
		u.Path += "/" + rel
	}
	return u.String()
}

// cleanRel normalisiert einen Pfad relativ zur Space-Wurzel ("", kein
// führender/trailing Slash, keine "..").
func cleanRel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "/")
	if s == "" || s == "." {
		return ""
	}
	parts := []string{}
	for _, seg := range strings.Split(s, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, "/")
}

// hrefPath dekodiert den Pfad eines DAV-href.
func hrefPath(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if u, err := url.Parse(href); err == nil && u.Path != "" {
		return u.Path
	}
	return href
}

func samePath(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

// okProp nimmt das Propstat mit HTTP 200 (404-Props für fehlende Felder
// ignorieren, wie das Web-UI).
func okProp(r davResponse) (davProp, bool) {
	for _, ps := range r.Propstats {
		if strings.Contains(ps.Status, " 200 ") || strings.HasSuffix(ps.Status, " 200") {
			return ps.Prop, true
		}
	}
	if len(r.Propstats) == 1 {
		return r.Propstats[0].Prop, true
	}
	return davProp{}, false
}

func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// isDirBody erkennt Ordner-Antworten auf GET (Multistatus-XML statt Datei).
func isDirBody(ctype string, body []byte) bool {
	ct := strings.ToLower(ctype)
	if strings.Contains(ct, "xml") {
		return true
	}
	snip := string(bytes.TrimSpace(body))
	if len(snip) > 300 {
		snip = snip[:300]
	}
	return strings.Contains(snip, "<d:multistatus") || strings.Contains(snip, "<multistatus")
}

func isHTML(ctype string, body []byte) bool {
	ct := strings.ToLower(ctype)
	if strings.Contains(ct, "text/html") {
		return true
	}
	if ct != "" && !strings.Contains(ct, "text/") && !strings.Contains(ct, "html") {
		return false
	}
	snip := strings.ToLower(string(bytes.TrimSpace(body)))
	if len(snip) > 200 {
		snip = snip[:200]
	}
	return strings.Contains(snip, "<html") || strings.Contains(snip, "<!doctype html")
}

// --- Download-Helfer (Dateiname/Ziel wie mebis fetch) ---

// dispositionName liest filename*=UTF-8"… oder filename="…" aus.
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
	seg := strings.TrimSpace(path.Base(u.Path))
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
		return filepath.Join(ziel, name), nil
	}
	if fi, err := os.Stat(ziel); err == nil && fi.IsDir() {
		return filepath.Join(ziel, name), nil
	}
	if dir := filepath.Dir(ziel); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", coreerrors.Wrap(coreerrors.CodeInternal, "bycs-drive", "zielordner anlegen fehlgeschlagen", err)
		}
	}
	return ziel, nil
}
