// Command schoolconnect: einheitliche Schnittstelle zu bayerischen Schulplattformen.
//
// Modi (alle nutzen dieselbe Plugin Runtime):
//
//	schoolconnect list                              alle Funktionen auflisten
//	schoolconnect tool <plugin> <func> [--p v ...] Funktion via CLI aufrufen
//	schoolconnect serve                             REST-API starten (:8080 / REST_ADDR)
//	schoolconnect mcp                               MCP-Server via stdio (JSON-RPC)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"schoolconnect/internal/adapters/cli"
	"schoolconnect/internal/adapters/mcp"
	"schoolconnect/internal/adapters/rest"
	"schoolconnect/internal/app"
	"schoolconnect/internal/core/config"
	"schoolconnect/internal/core/httpclient"
	"schoolconnect/internal/core/logging"

	bycsdrive "schoolconnect/plugins/bycs-drive"
	bycsmessenger "schoolconnect/plugins/bycs-messenger"
	lernplanbayern "schoolconnect/plugins/lernplan-bayern"
	"schoolconnect/plugins/mebis"
	"schoolconnect/plugins/schuelerportal"
)

func main() {
	cfg := config.Load()
	log := logging.New(cfg.LogLevel)
	ctx := context.Background()

	// Plugin Runtime aufbauen: neues Plugin = 1 Zeile Register(), sonst nichts.
	hc := httpclient.New()
	rt := app.New(log)
	rt.Register(schuelerportal.New(hc))
	rt.Register(mebis.New(hc))
	rt.Register(bycsdrive.New(hc))
	rt.Register(bycsmessenger.New(hc))
	rt.Register(lernplanbayern.New(hc))

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			srv := rest.New(rt, log)
			addr := cfg.RestAddr
			log.Info("REST listening", "addr", addr)
			fmt.Printf("REST: http://localhost%s/api  (z.B. /api/lernplan-bayern/search?query=Funktionen)\n", addr)
			if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
				fmt.Fprintln(os.Stderr, "serve error:", err)
				os.Exit(1)
			}
			return
		case "mcp":
			log.Info("MCP stdio serving")
			mcp.New(rt, log).ServeSTDIO(ctx)
			return
		}
	}
	os.Exit(cli.Run(ctx, rt, os.Args))
}
