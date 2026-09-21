// Package cli ist der CLI-Adapter: jede Plugin-Funktion wird automatisch
// ein CLI-Kommando:  tool <plugin-id> <funktionsname> [--param wert ...]
//
// Auth läuft generisch über die Runtime: Plugins deklarieren AuthParams,
// die Runtime erzeugt daraus "auth"/"logout"-Funktionen. Die CLI ergänzt
// Top-Level-Komfortbefehle:
//
//	schoolconnect auth <plugin> [--param wert ...]    fehlende Pflichtwerte werden interaktiv abgefragt
//	schoolconnect logout <plugin>                     gespeicherte Anmeldedaten verwerfen
//
// Fehlende Required-Params fragt die CLI interaktiv nach, sobald stdin ein
// Terminal ist (Secret-Params ohne Echo). Ohne TTY (Scripts, Pipes) kommt
// wie bisher ein Fehler — nichts blockiert. Prompts gehen immer auf stderr,
// stdout bleibt reines JSON.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"schoolconnect/internal/app"
	"schoolconnect/internal/core/tenant"
	"schoolconnect/internal/domain"
)

// Run parst os.Args gegen die Runtime-Funktionen.
// Der Tenant kommt aus --tenant <name> oder SC_TENANT (Default "default")
// und gilt für auth/logout/tool gleichermaßen — normale Einzelnutzung
// läuft ohne jede Angabe wie bisher.
func Run(ctx context.Context, rt *app.Runtime, args []string) int {
	ctx = withTenantFromArgs(ctx, args)
	if len(args) < 2 || args[1] == "help" || args[1] == "-h" {
		printHelp(rt)
		return 0
	}
	switch args[1] {
	case "list":
		fns := rt.Functions()
		sort.Slice(fns, func(i, j int) bool { return fns[i].FullName() < fns[j].FullName() })
		for _, fn := range fns {
			fmt.Printf("%-32s %s\n", fn.FullName(), fn.Description)
		}
		return 0
	case "auth":
		return runAuth(ctx, rt, args)
	case "logout":
		return runLogout(ctx, rt, args)
	case "tool":
		return runTool(ctx, rt, args)
	default:
		fmt.Fprintln(os.Stderr, "usage: schoolconnect auth <plugin> [--param value ...] [--tenant <name>]")
		fmt.Fprintln(os.Stderr, "       schoolconnect tool <plugin> <function> [--param value ...] [--tenant <name>]")
		fmt.Fprintln(os.Stderr, "       schoolconnect logout <plugin> [--tenant <name>]")
		return 2
	}
}

// runAuth: schoolconnect auth <plugin> [--param value ...].
// Fehlende Pflicht-Credentials werden interaktiv abgefragt — aber erst
// nach dem Env-Fallback der Runtime: Wer SCHUELERPORTAL_SECRET gesetzt hat,
// wird ohne TTY und ohne einen einzigen Prompt angemeldet.
func runAuth(ctx context.Context, rt *app.Runtime, args []string) int {
	if len(args) < 3 || strings.HasPrefix(args[2], "--") {
		fmt.Fprintln(os.Stderr, "usage: schoolconnect auth <plugin> [--param value ...]")
		return 2
	}
	pluginID := args[2]
	params, err := rt.AuthParamsFor(pluginID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(params) == 0 {
		fmt.Fprintf(os.Stderr, "plugin %q braucht keine Anmeldung\n", pluginID)
		return 0
	}
	parsed := parseFlags(args[3:])
	if err := fillMissingWithEnvFallback(rt, pluginID, params, parsed); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	res, err := rt.Call(ctx, pluginID, "auth", parsed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	printJSON(res)
	return 0
}

// runLogout: schoolconnect logout <plugin>.
func runLogout(ctx context.Context, rt *app.Runtime, args []string) int {
	if len(args) < 3 || strings.HasPrefix(args[2], "--") {
		fmt.Fprintln(os.Stderr, "usage: schoolconnect logout <plugin>")
		return 2
	}
	res, err := rt.Call(ctx, args[2], "logout", map[string]string{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	printJSON(res)
	return 0
}

// runTool: schoolconnect tool <plugin> <function> [--param value ...].
func runTool(ctx context.Context, rt *app.Runtime, args []string) int {
	if len(args) < 4 || strings.HasPrefix(args[2], "--") {
		fmt.Fprintln(os.Stderr, "usage: schoolconnect tool <plugin> <function> [--param value ...]")
		return 2
	}
	pluginID, funcName := args[2], args[3]
	if strings.HasPrefix(funcName, "--") {
		fmt.Fprintln(os.Stderr, "usage: schoolconnect tool <plugin> <function> [--param value ...]")
		return 2
	}
	parsed := parseFlags(args[4:])

	// Fehlende Pflicht-Params interaktiv nachfragen (nur mit TTY, sonst
	// meldet die Runtime den Fehler wie bisher). Auth-Params der Runtime
	// (Credentials) werden hier bewusst NICHT abgefragt: Sie kommen aus
	// Store/Env/Args via Runtime-Merge — nur "auth" fragt sie ab.
	if fn := findFunction(rt, pluginID, funcName); fn != nil && funcName != "auth" {
		if err := fillMissingInteractive(fn.Params, parsed); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	if funcName == "auth" {
		params, err := rt.AuthParamsFor(pluginID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if err := fillMissingWithEnvFallback(rt, pluginID, params, parsed); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}

	res, err := rt.Call(ctx, pluginID, funcName, parsed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	printJSON(res)
	return 0
}

func findFunction(rt *app.Runtime, pluginID, funcName string) *domain.Function {
	for _, fn := range rt.Functions() {
		if fn.PluginID == pluginID && fn.Name == funcName {
			fn := fn
			return &fn
		}
	}
	return nil
}

// withTenantFromArgs legt den Tenant in den Context: --tenant <name>
// (überall in der Arg-Liste, z.B. vor oder nach dem Subcommand) gewinnt
// gegen SC_TENANT; ohne Angabe gilt der Default-Tenant.
func withTenantFromArgs(ctx context.Context, args []string) context.Context {
	for i := 1; i < len(args); i++ {
		if args[i] == "--tenant" && i+1 < len(args) {
			return tenant.WithTenant(ctx, args[i+1])
		}
		if strings.HasPrefix(args[i], "--tenant=") {
			return tenant.WithTenant(ctx, strings.TrimPrefix(args[i], "--tenant="))
		}
	}
	if t := tenant.FromEnv(); t != "" {
		return tenant.WithTenant(ctx, t)
	}
	return tenant.WithTenant(ctx, tenant.Default)
}

// parseFlags parst --key value / --key=value (Rest ohne -- ist ein Fehler).
// --tenant wird hier herausgefiltert (gehört dem Adapter, nicht der Funktion).
func parseFlags(rest []string) map[string]string {
	parsed := map[string]string{}
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		if !strings.HasPrefix(tok, "--") {
			fmt.Fprintf(os.Stderr, "unexpected arg %q (expected --param value)\n", tok)
			continue
		}
		tok = strings.TrimPrefix(tok, "--")
		if tok == "tenant" {
			if i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "--") {
				i++
			}
			continue
		}
		if strings.HasPrefix(tok, "tenant=") {
			continue
		}
		if eq := strings.Index(tok, "="); eq >= 0 {
			parsed[tok[:eq]] = tok[eq+1:]
			continue
		}
		if i+1 < len(rest) && !strings.HasPrefix(rest[i+1], "--") {
			parsed[tok] = rest[i+1]
			i++
		} else {
			parsed[tok] = "true"
		}
	}
	return parsed
}

// fillMissingWithEnvFallback fragt fehlende Required-Auth-Params ab —
// aber nur, was nach Store + Env-Fallback (Runtime-Merge) noch fehlt.
// Wer z.B. SCHUELERPORTAL_SECRET gesetzt hat, wird ohne einen Prompt
// angemeldet; ohne TTY kommt statt zu blockieren ein Fehler.
func fillMissingWithEnvFallback(rt *app.Runtime, pluginID string, params []domain.Param, args map[string]string) error {
	preview, err := rt.PreviewMerge(pluginID, args)
	if err != nil {
		return err
	}
	for _, p := range params {
		if !p.Required || strings.TrimSpace(preview[p.Name]) != "" {
			continue
		}
		if p.Default != "" {
			args[p.Name] = p.Default
			continue
		}
		if !isTerminal(os.Stdin) {
			aliasHint := ""
			if len(p.Aliases) > 0 {
				aliasHint = " (Aliase: --" + strings.Join(p.Aliases, ", --") + ")"
			}
			return fmt.Errorf("missing credential %q%s (per --%s übergeben, per Env setzen oder interaktiv aufrufen)", p.Name, aliasHint, p.Name)
		}
		val, err := prompt(p)
		if err != nil {
			return err
		}
		if strings.TrimSpace(val) == "" && p.Default != "" {
			val = p.Default
		}
		args[p.Name] = val
	}
	return nil
}

// fillMissingInteractive fragt fehlende Required-Params auf stderr ab.
// Params mit Default gelten als erfüllt (Default wird bei leerer Eingabe
// übernommen); bekannte Aliase zählen als gesetzt. Ohne TTY an stdin
// kommt ein Fehler statt zu blockieren.
func fillMissingInteractive(params []domain.Param, args map[string]string) error {
	for _, p := range params {
		if !p.Required || hasValue(params, p.Name, args) {
			continue
		}
		if p.Default != "" {
			args[p.Name] = p.Default
			continue
		}
		if !isTerminal(os.Stdin) {
			aliasHint := ""
			if len(p.Aliases) > 0 {
				aliasHint = " (Aliase: --" + strings.Join(p.Aliases, ", --") + ")"
			}
			return fmt.Errorf("missing required param %q%s (per --%s übergeben oder interaktiv aufrufen)", p.Name, aliasHint, p.Name)
		}
		val, err := prompt(p)
		if err != nil {
			return err
		}
		if strings.TrimSpace(val) == "" && p.Default != "" {
			val = p.Default
		}
		args[p.Name] = val
	}
	return nil
}

// hasValue prüft kanonischen Namen und Aliase.
func hasValue(params []domain.Param, name string, args map[string]string) bool {
	if strings.TrimSpace(args[name]) != "" {
		return true
	}
	for _, p := range params {
		if p.Name != name {
			continue
		}
		for _, a := range p.Aliases {
			if strings.TrimSpace(args[a]) != "" {
				return true
			}
		}
	}
	return false
}

func prompt(p domain.Param) (string, error) {
	label := p.Name
	if p.Description != "" {
		label = fmt.Sprintf("%s (%s)", p.Name, p.Description)
	}
	if p.Secret {
		return readSecret(label + ": ")
	}
	fmt.Fprintf(os.Stderr, "%s: ", label)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readSecret liest eine Zeile ohne Echo (stdlib-only via stty;
// Fallback: sichtbare Eingabe mit Warnung).
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	if !isTerminal(os.Stdin) {
		line, err := r.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	off := exec.Command("stty", "-echo")
	off.Stdin = os.Stdin
	if err := off.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "(Warnung: Eingabe ist sichtbar)")
		line, rerr := r.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), rerr
	}
	defer func() {
		on := exec.Command("stty", "echo")
		on.Stdin = os.Stdin
		_ = on.Run()
		fmt.Fprintln(os.Stderr)
	}()
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func printHelp(rt *app.Runtime) {
	fmt.Println("schoolconnect — einheitliche Schnittstelle zu bayerischen Schulplattformen")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  schoolconnect list                              alle Funktionen auflisten")
	fmt.Println("  schoolconnect auth <plugin> [--p v ...]         anmelden (Fehlendes wird abgefragt)")
	fmt.Println("  schoolconnect logout <plugin>                   Anmeldedaten verwerfen")
	fmt.Println("  schoolconnect tool <plugin> <func> [--p v ...]  Funktion aufrufen")
	fmt.Println()
	fmt.Println("Tenant (optional, Default \"default\"):")
	fmt.Println("  --tenant <name>  oder Env SC_TENANT   trennt gespeicherte Logins + Sessions je Tenant")
	fmt.Println()
	fmt.Println("Plugins / Funktionen:")
	fns := rt.Functions()
	sort.Slice(fns, func(i, j int) bool { return fns[i].FullName() < fns[j].FullName() })
	for _, fn := range fns {
		fmt.Printf("  tool %-28s %s\n", fn.FullName(), fn.Description)
		params := []string{}
		for _, p := range fn.Params {
			mark := ""
			if p.Required {
				mark = " (required)"
			}
			if p.Secret {
				mark += " (secret)"
			}
			names := "--" + p.Name
			for _, a := range p.Aliases {
				names += "|--" + a
			}
			params = append(params, names+mark)
		}
		if len(params) > 0 {
			fmt.Printf("      params: %s\n", strings.Join(params, ", "))
		}
	}
}
