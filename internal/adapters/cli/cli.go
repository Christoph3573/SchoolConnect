// Package cli ist der CLI-Adapter: jede Plugin-Funktion wird automatisch
// ein CLI-Kommando:  tool <plugin-id> <funktionsname> [--param wert ...]
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"schoolconnect/internal/app"
)

// Run parst os.Args gegen die Runtime-Funktionen.
func Run(ctx context.Context, rt *app.Runtime, args []string) int {
	if len(args) < 2 || args[1] == "help" || args[1] == "-h" {
		printHelp(rt)
		return 0
	}
	if args[1] == "list" {
		fns := rt.Functions()
		sort.Slice(fns, func(i, j int) bool { return fns[i].FullName() < fns[j].FullName() })
		for _, fn := range fns {
			fmt.Printf("%-32s %s\n", fn.FullName(), fn.Description)
		}
		return 0
	}
	if args[1] != "tool" {
		fmt.Fprintln(os.Stderr, "usage: schoolconnect tool <plugin> <function> [--param value ...]")
		return 2
	}
	if len(args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: schoolconnect tool <plugin> <function> [--param value ...]")
		return 2
	}
	pluginID, funcName := args[2], args[3]

	// Restliche Args als --key value / --key=value parsen.
	parsed := map[string]string{}
	fs := flag.NewFlagSet("tool", flag.ContinueOnError)
	// Zwei Passes: erst Flags dynamisch registrieren geht ohne Schema nicht,
	// daher manuell parsen.
	rest := args[4:]
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		if !strings.HasPrefix(tok, "--") {
			fmt.Fprintf(os.Stderr, "unexpected arg %q (expected --param value)\n", tok)
			return 2
		}
		tok = strings.TrimPrefix(tok, "--")
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
	_ = fs

	res, err := rt.Call(ctx, pluginID, funcName, parsed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
	return 0
}

func printHelp(rt *app.Runtime) {
	fmt.Println("schoolconnect — einheitliche Schnittstelle zu bayerischen Schulplattformen")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  schoolconnect list                              alle Funktionen auflisten")
	fmt.Println("  schoolconnect tool <plugin> <func> [--p v ...]  Funktion aufrufen")
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
			params = append(params, "--"+p.Name+mark)
		}
		if len(params) > 0 {
			fmt.Printf("      params: %s\n", strings.Join(params, ", "))
		}
	}
}
