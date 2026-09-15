// Package lernplanbayern kapselt LehrplanPLUS Bayern
// (https://www.lehrplanplus.bayern.de).
//
// search löst Schulart/Fach/Jahrgangsstufe/Kapitel auf IDs auf,
// ruft GET /suche/kapitel auf und parst die Trefferliste
// (<ul class="results">). Zurück kommt count, kap und die
// Ergebnis-URL(s), z.B. count=1, url="/fachlehrplan/gymnasium/9/deutsch".
package lernplanbayern

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	coreerrors "schoolconnect/internal/core/errors"
	"schoolconnect/internal/domain"
)

// Plugin implementiert domain.Plugin.
type Plugin struct {
	client *http.Client
	token  string
}

// New erzeugt das Plugin (Core stellt den HTTP-Client).
func New(client *http.Client) *Plugin { return &Plugin{client: client} }

func (p *Plugin) ID() string          { return "lernplan-bayern" }
func (p *Plugin) Name() string        { return "Lernplan Bayern" }
func (p *Plugin) Description() string { return "LehrplanPLUS Bayern: Fachlehrpläne, Fachprofile und Kompetenzen suchen." }

func (p *Plugin) Authenticate(_ context.Context, credentials map[string]string) error {
	// Öffentliche Inhalte: kein Login nötig; Methode bleibt für Einheitlichkeit.
	if t, ok := credentials["token"]; ok {
		p.token = t
	}
	return nil
}

// --- Lookup-Tabellen (Stand: Filter-Options aus /wizard) ---

const baseURL = "https://www.lehrplanplus.bayern.de"

var schulartByName = map[string]string{
	"grundschule":    "24427",
	"mittelschule":   "24428",
	"förderschule":   "24429",
	"foerderschule":  "24429",
	"realschule":     "24430",
	"gymnasium":      "24431",
	"wirtschaftsschule": "24432",
	"fachoberschule": "43644",
	"berufsoberschule": "43645",
}

var schulartByID = map[string]string{
	"24427": "Grundschule",
	"24428": "Mittelschule",
	"24429": "Förderschule",
	"24430": "Realschule",
	"24431": "Gymnasium",
	"24432": "Wirtschaftsschule",
	"43644": "Fachoberschule",
	"43645": "Berufsoberschule",
}

var kapitelByID = map[string]string{
	"kap0":     "Leitlinien",
	"kap1":     "Bildungs- und Erziehungsauftrag",
	"kap2fuez": "Übergreifende Bildungs- und Erziehungsziele",
	"kap2":     "Fachprofile",
	"kap3":     "Grundlegende Kompetenzen (Jahrgangsstufenprofile)",
	"kap4":     "Fachlehrpläne",
}

var jgsNameByID = map[string]string{
	"23844": "1", "23847": "2", "23846": "3", "23845": "4",
	"23848": "5", "23849": "6", "23854": "7", "23853": "8",
	"23852": "9", "23851": "10", "23850": "11", "23843": "12",
	"23855": "13",
}

var jgsIDByName = map[string]string{
	"1": "23844", "2": "23847", "3": "23846", "4": "23845",
	"5": "23848", "6": "23849", "7": "23854", "8": "23853",
	"9": "23852", "10": "23851", "11": "23850", "12": "23843",
	"13": "23855",
}

// fachNameByID: ID -> Anzeigename (vollständige Optionsliste aus /wizard).
var fachNameByID = map[string]string{
	"314527": "Archäologie",
	"374721": "Aspekte der Chemie",
	"374718": "Aspekte der Physik",
	"374722": "Aspekte der Psychologie",
	"171072": "Ästhetische Bildung",
	"248941": "Berufliche Orientierung",
	"374716": "Berufs- und Lebensorientierung – Praxis",
	"171070": "Berufs- und Lebensorientierung – Praxis Ernährung und Soziales",
	"171071": "Berufs- und Lebensorientierung – Praxis Technik",
	"171054": "Berufs- und Lebensorientierung – Theorie",
	"231418": "Beruf und Arbeit",
	"23867":  "Betriebswirtschaftliche Steuerung und Kontrolle",
	"374717": "Betriebswirtschaftslehre",
	"23892":  "Betriebswirtschaftslehre / Rechnungswesen",
	"23886":  "Biologie",
	"23903":  "Biologisch-chemisches Praktikum",
	"91516":  "Biotechnologie",
	"171053": "Blindenkurzschrift",
	"171055": "Blindheit und Lebenspraxis",
	"23891":  "Buchführung",
	"23908":  "Chemie",
	"23913":  "Chinesisch",
	"23885":  "Deutsch",
	"23884":  "Deutsch als Zweitsprache",
	"171052": "Deutsche Gebärdensprache",
	"296423": "Digitale Bildung (FS)",
	"289113": "Digitale Bildung (WS)",
	"361910": "E-Commerce",
	"23900":  "Englisch",
	"203111": "English Book Club",
	"23868":  "Ernährung und Gesundheit",
	"23904":  "Ernährung und Soziales",
	"23883":  "Ethik",
	"23926":  "Evangelische Religionslehre",
	"98389":  "Experimentelles Gestalten",
	"43646":  "Fachpraktische Ausbildung",
	"361903": "Fit for Finance",
	"23856":  "Französisch",
	"231419": "Freizeit",
	"361904": "Gamification",
	"23882":  "Geographie",
	"23914":  "Geologie",
	"23881":  "Geschichte",
	"24226":  "Geschichte/Politik/Geographie",
	"171069": "Geschichte/Politik/Geographie und Natur und Technik (FS)",
	"91575":  "Geschichte/Politik und Gesellschaft (FOS/BOS)",
	"23902":  "Geschichte/Politik und Gesellschaft (WS)",
	"43641":  "Gestaltung",
	"361905": "Gesundheit",
	"108281": "Gesundheitswirtschaft und Recht",
	"108284": "Gesundheitswissenschaften",
	"23874":  "Griechisch",
	"231420": "Grundlegender entwicklungsbezogener Unterricht",
	"23931":  "Heimat- und Sachunterricht",
	"23928":  "Informatik",
	"209599": "Informatik und digitales Gestalten",
	"171073": "Informations- und Kommunikationstechnische Bildung",
	"24072":  "Informationstechnologie",
	"23932":  "Informationsverarbeitung",
	"314548": "Instrumentalensemble",
	"108255": "International Business Studies",
	"108256": "Internationale Betriebswirtschaftslehre und Volkswirtschaftslehre",
	"43642":  "Internationale Politik",
	"292205": "Islamischer Unterricht",
	"337043": "Israelitische Religionslehre",
	"23857":  "Italienisch",
	"23927":  "Katholische Religionslehre",
	"108287": "Kommunikation und Interaktion",
	"23894":  "Kunst",
	"374720": "Künstliche Intelligenz, Informatik und Technologie (KIT)",
	"374719": "Künstliche Intelligenz und Wirtschaftsinformatik (KIWI)",
	"23873":  "Latein",
	"231421": "Leben in der Gesellschaft",
	"361906": "Life Skills",
	"23880":  "Mathematik",
	"43731":  "Medien",
	"336976": "Mensch, Umwelt, Technik",
	"24110":  "Mensch und Umwelt",
	"231422": "Mobilität",
	"23879":  "Musik",
	"23901":  "Musisch-ästhetische Bildung",
	"23877":  "Natur und Technik",
	"23939":  "Natur und Technik (Gym)",
	"43732":  "Naturwissenschaften",
	"350738": "Ökonomische Bildung",
	"336977": "Ökonomische Bildung und Digitale Bildung",
	"289112": "Ökonomische Grundlagen",
	"300692": "Orthodoxe Religionslehre",
	"43733":  "Pädagogik/Psychologie",
	"231423": "Persönlichkeit und soziale Beziehungen",
	"374715": "Pharmazeutische und medizinische Chemie",
	"23878":  "Physik",
	"211404": "Politik und Gesellschaft",
	"23917":  "Polnisch",
	"314549": "Psychologie",
	"43730":  "Rechtslehre",
	"171057": "Rhythmik und Musik",
	"361907": "Robotik",
	"23859":  "Russisch",
	"227649": "Sach- und lebensbezogener Unterricht",
	"248940": "Soziallehre",
	"23860":  "Sozialpraktische Grundbildung",
	"91657":  "Sozialpsychologie",
	"23861":  "Sozialwesen",
	"43729":  "Sozialwirtschaft und Recht",
	"23893":  "Sozialwissenschaftliche Arbeitsfelder",
	"43734":  "Soziologie",
	"23858":  "Spanisch",
	"108279": "Spektrum der Gesundheit",
	"23876":  "Sport",
	"227650": "Sport und Bewegung",
	"314550": "Sport und Gesellschaft",
	"91637":  "Studier- und Arbeitstechniken",
	"314551": "Tanz- und Bewegungskünstetheater",
	"98388":  "Tastschreiben (Lehrgang)",
	"23905":  "Technik",
	"43639":  "Technologie",
	"23864":  "Textiles Gestalten",
	"314552": "Theater und Film",
	"361908": "Tourismus",
	"23918":  "Tschechisch",
	"23919":  "Türkisch",
	"24111":  "Übungsunternehmen",
	"361911": "Umweltökonomie",
	"361909": "Umwelttechnik",
	"314553": "Vokalensemble",
	"43728":  "Volkswirtschaftslehre",
	"23863":  "Werken",
	"23865":  "Werken und Gestalten",
	"171056": "Werken und Gestalten / Kunst",
	"91619":  "Wirtschaft Aktuell",
	"23933":  "Wirtschaftsgeographie",
	"23875":  "Wirtschaftsinformatik",
	"23887":  "Wirtschaft und Beruf",
	"23906":  "Wirtschaft und Kommunikation",
	"23866":  "Wirtschaft und Recht",
	"334100": "Wissenschaftspropädeutisches Seminar",
	"231424": "Wohnen",
}

var fachIDByName map[string]string

func init() {
	fachIDByName = make(map[string]string, len(fachNameByID))
	for id, name := range fachNameByID {
		fachIDByName[strings.ToLower(name)] = id
	}
}

// --- Functions ---

func (p *Plugin) Functions() []domain.Function {
	return []domain.Function{
		{
			Name:        "search",
			Description: "LehrplanPLUS-Kapitel suchen: Schulart, Kapitel, Fach und Jahrgangsstufe sind Pflicht. Liefert Treffer-URLs (z.B. /fachlehrplan/gymnasium/9/deutsch).",
			Params: []domain.Param{
				{Name: "schulart", Description: "Schulart: Name (z.B. Gymnasium) oder ID (z.B. 24431).", Required: true},
				{Name: "lehrplankapitel", Description: "Lehrplankapitel: kap0..kap4/kap2fuez oder Name (z.B. kap4 / Fachlehrpläne).", Required: true},
				{Name: "fach", Description: "Fach: Name (z.B. Deutsch) oder ID (z.B. 23885).", Required: true},
				{Name: "jahrgangsstufe", Description: "Jahrgangsstufe: 1..13 oder ID (z.B. 9).", Required: true},
			},
			Handler: p.search,
		},
		{
			Name:        "details",
			Description: "Lehrplan-Seite laden (URL aus search). Liefert Inhaltsverzeichnis + Abschnitte mit Kompetenzerwartungen plus Halbjahr-Check (ob die Seite 13/1 vs. 13/2 unterscheidet).",
			Params: []domain.Param{
				{Name: "url", Description: "Ergebnis-URL aus search (relativ oder absolut).", Required: true},
				{Name: "abschnitt", Description: "Optional: nur passende Abschnitte (Code wie '1.1' oder Titelstichwort wie 'Sprechen')."},
			},
			Handler: p.details,
		},
	}
}

// --- Resolver: Klarname oder ID -> ID ---

func resolveSchulart(in string) (id, name string, err error) {
	s := strings.TrimSpace(in)
	if n, ok := schulartByID[s]; ok {
		return s, n, nil
	}
	if id, ok := schulartByName[strings.ToLower(s)]; ok {
		return id, schulartByID[id], nil
	}
	return "", "", fmt.Errorf("unbekannte schulart %q (z.B. Gymnasium oder 24431)", in)
}

func resolveKapitel(in string) (id, name string, err error) {
	s := strings.ToLower(strings.TrimSpace(in))
	for id, name := range kapitelByID {
		if s == strings.ToLower(id) {
			return id, name, nil
		}
	}
	// Klarnamen / Teiltreffer zulassen ("Fachlehrpläne", "Fachprofile", ...)
	for id, name := range kapitelByID {
		if s == strings.ToLower(name) || strings.Contains(strings.ToLower(name), s) || strings.Contains(s, strings.ToLower(name)) {
			return id, name, nil
		}
	}
	return "", "", fmt.Errorf("unbekanntes lehrplankapitel %q (z.B. kap4 / Fachlehrpläne)", in)
}

func resolveFach(in string) (id, name string, err error) {
	s := strings.TrimSpace(in)
	if n, ok := fachNameByID[s]; ok {
		return s, n, nil
	}
	if id, ok := fachIDByName[strings.ToLower(s)]; ok {
		return id, fachNameByID[id], nil
	}
	return "", "", fmt.Errorf("unbekanntes fach %q (z.B. Deutsch oder 23885)", in)
}

func resolveJgs(in string) (id, name string, err error) {
	s := strings.ToLower(strings.TrimSpace(in))
	s = strings.TrimPrefix(s, "jahrgangsstufe ")
	s = strings.TrimPrefix(s, "jahrgang ")
	s = strings.TrimPrefix(s, "jgs ")
	s = strings.TrimSpace(s)
	if n, ok := jgsNameByID[s]; ok {
		return s, n, nil
	}
	if id, ok := jgsIDByName[s]; ok {
		return id, s, nil
	}
	return "", "", fmt.Errorf("unbekannte jahrgangsstufe %q (1..13)", in)
}

// --- search: GET /suche/kapitel + Ergebnis-URLs parsen ---

var (
	resultsBlockRe = regexp.MustCompile(`(?s)<ul class="results">(.*?)</ul>`)
	resultLinkRe   = regexp.MustCompile(`(?s)<a\s+href="([^"]+)"[^>]*>\s*<h2[^>]*>(.*?)</h2>(.*?)</a>`)
	tagRe          = regexp.MustCompile(`<[^>]+>`)
	wsRe           = regexp.MustCompile(`\s+`)
)

func cleanText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = wsRe.ReplaceAllString(strings.TrimSpace(s), " ")
	return s
}

// kapitelNutzt steuert, welche Filter die Suche je Kapitel mitschickt
// (data-usable aus /wizard: kap4 braucht alle, kap2 kein jgs, kap3 kein fach).
func kapitelNutzt(kapID, filter string) bool {
	switch kapID {
	case "kap4":
		return true // schulart, fach, jgs
	case "kap2":
		return filter != "jgs"
	case "kap3":
		return filter != "fach"
	case "kap1":
		return filter == "schulart"
	case "kap0":
		return filter == "schulart"
	case "kap2fuez":
		return false
	default:
		return true
	}
}

func (p *Plugin) search(ctx context.Context, args map[string]string) (any, error) {
	schulartID, schulartName, err := resolveSchulart(args["schulart"])
	if err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	kapID, kapName, err := resolveKapitel(args["lehrplankapitel"])
	if err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	fachID, fachName, err := resolveFach(args["fach"])
	if err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}
	jgsID, jgsName, err := resolveJgs(args["jahrgangsstufe"])
	if err != nil {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), err.Error())
	}

	q := url.Values{}
	q.Set("schulart[]", schulartID)
	q.Set("lehrplankapitel[]", kapID)
	if kapitelNutzt(kapID, "fach") {
		q.Set("fach[]", fachID)
	}
	if kapitelNutzt(kapID, "jgs") {
		q.Set("jgs[]", jgsID)
	}
	reqURL := baseURL + "/suche/kapitel?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "SchoolConnect/1.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "lehrplanplus nicht erreichbar", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort lesen fehlgeschlagen", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("lehrplanplus antwortet mit HTTP %d", resp.StatusCode))
	}
	page := string(body)

	results := []any{}
	if m := resultsBlockRe.FindStringSubmatch(page); m != nil {
		for _, lm := range resultLinkRe.FindAllStringSubmatch(m[1], -1) {
			rel := html.UnescapeString(lm[1])
			titel := cleanText(lm[2])
			meta := cleanText(lm[3])
			full := rel
			if strings.HasPrefix(rel, "/") {
				full = baseURL + rel
			}
			results = append(results, map[string]any{
				"titel": titel, "meta": meta, "url": rel, "full_url": full,
			})
		}
	}

	topURL := ""
	if len(results) > 0 {
		topURL = results[0].(map[string]any)["url"].(string)
	}

	return map[string]any{
		"count":          len(results),
		"kap":            kapID,
		"kapitel":        kapName,
		"url":            topURL, // bei count=1 die Treffer-URL, sonst erster Treffer
		"schulart":       schulartName,
		"fach":           fachName,
		"jahrgangsstufe": jgsName,
		"results":        results,
		"_debug": map[string]any{
			"schulart_id": schulartID, "fach_id": fachID,
			"jgs_id": jgsID, "request_url": reqURL,
		},
	}, nil
}

// details lädt eine Ergebnis-URL und parst die Inhaltsstruktur:
// Inhaltsverzeichnis + Abschnitte (h2/h3) mit Fließtext und den
// Kompetenzerwartungen (Listenpunkte unter "Die Schülerinnen und Schüler ...").
// Optional filtert abschnitt (Code "1.1" oder Titelstichwort).
func (p *Plugin) details(ctx context.Context, args map[string]string) (any, error) {
	u := strings.TrimSpace(args["url"])
	if u == "" {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(), "missing required param \"url\"")
	}
	if strings.HasPrefix(u, "/") {
		u = baseURL + u
	}
	if !strings.HasPrefix(u, baseURL) {
		return nil, coreerrors.New(coreerrors.CodeBadRequest, p.ID(),
			"url muss auf www.lehrplanplus.bayern.de zeigen")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeInternal, p.ID(), "request bauen fehlgeschlagen", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "SchoolConnect/1.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "lehrplanplus nicht erreichbar", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, coreerrors.Wrap(coreerrors.CodeUpstream, p.ID(), "antwort lesen fehlgeschlagen", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, coreerrors.New(coreerrors.CodeUpstream, p.ID(),
			fmt.Sprintf("lehrplanplus antwortet mit HTTP %d", resp.StatusCode))
	}
	page := string(body)

	title := ""
	if m := titleRe.FindStringSubmatch(page); m != nil {
		title = cleanText(m[1])
	}
	h1 := ""
	if m := h1Re.FindStringSubmatch(page); m != nil {
		h1 = cleanText(m[1])
	}

	inhalt := parseInhaltsverzeichnis(page)
	abschnitte := parseAbschnitte(page)

	filter := strings.ToLower(strings.TrimSpace(args["abschnitt"]))
	if filter != "" {
		gefiltert := []any{}
		for _, a := range abschnitte {
			m, _ := a.(map[string]any)
			hay := strings.ToLower(fmt.Sprintf("%v %v %v", m["code"], m["titel"], m["uebergeordnet"]))
			if strings.Contains(hay, filter) {
				gefiltert = append(gefiltert, a)
			}
		}
		abschnitte = gefiltert
	}

	// Kompakter Textauszug für schnelle Übersicht (erster Abschnitt angerissen).
	kurz := ""
	if len(abschnitte) > 0 {
		if m, ok := abschnitte[0].(map[string]any); ok {
			kurz, _ = m["titel"].(string)
			if t, _ := m["text"].(string); t != "" {
				if len(t) > 800 {
					t = t[:800] + "…"
				}
				kurz = kurz + ": " + t
			}
		}
	}

	return map[string]any{
		"url": u, "title": title, "h1": h1,
		"inhaltsverzeichnis": inhalt,
		"abschnitte_count":   len(abschnitte),
		"abschnitte":         abschnitte,
		"halbjahr":           checkHalbjahr(page),
		"kurz":               kurz,
	}, nil
}

// halbjahrMuster sucht seitenweit nach Halbjahr-/Semester-Gliederung
// (z.B. "13/1", "13.1", "1. Halbjahr", "Semester"). Treffer im SVG-Logo
// (CSS-Pfade wie "h2.49c...") werden ausgeschlossen, indem nur Text
// außerhalb von <style>/<script>/<svg> und Attributen zählt.
var halbjahrMuster = regexp.MustCompile(`(?i)(halbjahr|semester|\b1\.\s*halbjahr|\b2\.\s*halbjahr|13\s*/\s*1|13\s*/\s*2|13\.1|13\.2|erstes\s+halbjahr|zweites\s+halbjahr)`)

// checkHalbjahr prüft, ob die Seite nach Halbjahren gliedert, und meldet
// Fundstellen (Textkontext) zurück — damit "erstes Halbjahr" beantwortbar ist,
// ohne die Seite manuell mit curl/grep zu durchsuchen.
func checkHalbjahr(page string) map[string]any {
	// Rauschen entfernen: Skripte, Styles, SVGs, Dialoge, Bilder.
	// (RE2/go-regexp kennt keine Rückreferenzen, daher je Tag einzeln.)
	strip := page
	for _, tag := range []string{"script", "style", "svg", "dialog"} {
		strip = regexp.MustCompile(`(?s)<`+tag+`[^>]*>.*?</`+tag+`>`).ReplaceAllString(strip, " ")
	}
	strip = imgRe.ReplaceAllString(strip, " ")
	text := cleanText(strip)
	funde := []string{}
	for _, m := range halbjahrMuster.FindAllStringSubmatch(text, -1) {
		treffer := strings.TrimSpace(m[0])
		if treffer == "" {
			continue
		}
		i := strings.Index(text, treffer)
		von := max(0, i-80)
		bis := min(len(text), i+len(treffer)+80)
		ctx := strings.TrimSpace(text[von:bis])
		dup := false
		for _, f := range funde {
			if f == ctx {
				dup = true
				break
			}
		}
		if !dup {
			funde = append(funde, ctx)
		}
		if len(funde) >= 10 {
			break
		}
	}
	return map[string]any{
		"gefunden":     len(funde) > 0,
		"treffer":      len(funde),
		"fundstellen":  funde,
		"gliederung":   jahrgangsGliederung(text),
		"halbjahr_13_1": filterHalbjahrAbschnitte(text, "13/1", "13.1", "erstes halbjahr", "1. halbjahr"),
	}
}

// jahrgangsGliederung erkennt, wonach die Seite gliedert (Jahrgang vs. Halbjahr).
func jahrgangsGliederung(text string) string {
	if halbjahrMuster.MatchString(text) {
		return "halbjahr"
	}
	if regexp.MustCompile(`(?i)(jahrgangsstufe|jahrgang|klasse)`).MatchString(text) {
		return "jahrgang"
	}
	return "unbekannt"
}

// filterHalbjahrAbschnitte sucht Textblöcke, die explizit 13/1 zugeordnet sind.
func filterHalbjahrAbschnitte(text string, marker ...string) []string {
	out := []string{}
	lower := strings.ToLower(text)
	for _, mk := range marker {
		idx := 0
		for {
			i := strings.Index(lower[idx:], strings.ToLower(mk))
			if i < 0 {
				break
			}
			i += idx
			von := max(0, i-200)
			bis := min(len(text), i+len(mk)+200)
			out = append(out, strings.TrimSpace(text[von:bis]))
			idx = i + len(mk)
			if len(out) >= 5 {
				return out
			}
		}
	}
	return out
}

// tocEntry ist ein Eintrag im Inhaltsverzeichnis (#flyer).
type tocEntry struct {
	Anchor string `json:"anchor"`
	Titel  string `json:"titel"`
}

var (
	// Inhaltsverzeichnis: <nav id="flyer"> ... <a href="#123">Titel</a>
	flyerBlockRe = regexp.MustCompile(`(?s)<nav id="flyer".*?>(.*?)</nav>`)
	flyerLinkRe  = regexp.MustCompile(`<a\s+href="#([^"]+)"[^>]*>(.*?)</a>`)
	// Abschnitte: ab <div id="content__sections">, getrennt an <section>.
	sectionsBlockRe = regexp.MustCompile(`(?s)<div id="content__sections">(.*)`)
	sectionSplitRe  = regexp.MustCompile(`<section>`)
	headerIDRe      = regexp.MustCompile(`<div class="header" id="([^"]+)">`)
	headingRe       = regexp.MustCompile(`(?s)<h([234])[^>]*>(.*?)</h[234]>`)
	contentBlockRe  = regexp.MustCompile(`(?s)<div class="content">(.*?)</div>\s*(?:<div class="footer">|</section>)`)
	paraRe          = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
	listItemRe      = regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`)
	dialogRe        = regexp.MustCompile(`(?s)<dialog.*?</dialog>`)
	imgRe           = regexp.MustCompile(`(?s)<img[^>]*>`)
	titleRe         = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
	h1Re            = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>`)
)

// parseInhaltsverzeichnis liest die Gliederung aus <nav id="flyer">.
func parseInhaltsverzeichnis(page string) []any {
	out := []any{}
	m := flyerBlockRe.FindStringSubmatch(page)
	if m == nil {
		return out
	}
	for _, lm := range flyerLinkRe.FindAllStringSubmatch(m[1], -1) {
		out = append(out, map[string]any{"anchor": lm[1], "titel": cleanText(lm[2])})
	}
	return out
}

// parseAbschnitte zerlegt die Content-Sections in strukturierte Einträge:
// code (z.B. "1.1"), titel, ebene (2/3), text (Fließtext) und
// kompetenzen (Listenpunkte unter "Kompetenzerwartungen und Inhalte").
func parseAbschnitte(page string) []any {
	out := []any{}
	m := sectionsBlockRe.FindStringSubmatch(page)
	if m == nil {
		return out
	}
	// Auf <footer id="footer"> bzw. Seitenende begrenzen.
	seg := m[1]
	if i := strings.Index(seg, `<footer id="footer">`); i >= 0 {
		seg = seg[:i]
	}
	parts := sectionSplitRe.Split(seg, -1)
	parent := ""
	for _, part := range parts {
		hm := headingRe.FindStringSubmatch(part)
		if hm == nil {
			continue
		}
		ebene := hm[1]
		heading := cleanText(hm[2])
		code, titel := splitCodeTitel(heading)
		if ebene == "2" {
			parent = titel
		}

		id := ""
		if im := headerIDRe.FindStringSubmatch(part); im != nil {
			id = im[1]
		}

		// Content-Block: Dialoge/Bilder raus, dann <p> und <li> sammeln.
		textParts := []string{}
		kompetenzen := []string{}
		if cm := contentBlockRe.FindStringSubmatch(part + "</section>"); cm != nil {
			content := dialogRe.ReplaceAllString(cm[1], "")
			content = imgRe.ReplaceAllString(content, "")
			// Kompetenzlisten: <ul> nach "Kompetenzerwartungen"-h4.
			for _, li := range listItemRe.FindAllStringSubmatch(content, -1) {
				t := cleanText(li[1])
				if t == "" || strings.HasPrefix(t, "Abschnitt hinzufügen") {
					continue
				}
				kompetenzen = append(kompetenzen, t)
			}
			for _, pm := range paraRe.FindAllStringSubmatch(content, -1) {
				t := cleanText(pm[1])
				if t == "" {
					continue
				}
				textParts = append(textParts, t)
			}
		}
		// <li> ohne umgebende <ul> (selten) fallen raus — Duplikate vermeiden:
		// Text aus Absätzen hat Vorrang; Kompetenzen bleiben separat.
		text := strings.Join(textParts, "\n")

	var uebergeordnet string
		if ebene == "2" {
			titel = heading // Lernbereichs-Titel vollständig behalten ("D9 Lernbereich 1: ...")
			code = ""
		} else {
			uebergeordnet = parent
		}

		out = append(out, map[string]any{
			"id": id, "ebene": ebene, "code": code, "titel": titel,
			"uebergeordnet": uebergeordnet,
			"text": text, "kompetenzen": kompetenzen,
		})
	}
	return out
}

// splitCodeTitel trennt "D9 1.1 Verstehend zuhören" -> ("1.1", "Verstehend zuhören")
// bzw. "1 Sprechen und Zuhören" -> ("1", "Sprechen und Zuhören").
func splitCodeTitel(heading string) (code, titel string) {
	f := strings.Fields(heading)
	// Fachkürzel wie "D9" vorne abwerfen.
	if len(f) > 0 && regexp.MustCompile(`^[A-ZÄÖÜ]+\d+$`).MatchString(f[0]) {
		f = f[1:]
	}
	if len(f) == 0 {
		return "", heading
	}
	if regexp.MustCompile(`^[\d.]+$`).MatchString(f[0]) {
		return f[0], strings.Join(f[1:], " ")
	}
	return "", heading
}
