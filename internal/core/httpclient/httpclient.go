// Package httpclient stellt den geteilten HTTP-Client für alle Plugins.
package httpclient

import (
	"net/http"
	"time"
)

// New erzeugt einen Client mit sinnvollen Timeouts für Schul-APIs.
func New() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}
