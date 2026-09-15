// Package config lädt Konfiguration & Secrets aus Env-Variablen.
package config

import (
	"os"
)

// Config ist die globale Anwendungskonfiguration.
type Config struct {
	LogLevel string
	RestAddr string
	// PluginSecrets hält rohe Credential-Strings je Plugin-ID,
	// z.B. MEbis_TOKEN, SCHUELERPORTAL_USER ... (Plugins parsen selbst).
	PluginSecrets map[string]string
}

// Load liest Env (mit sinnvollen Defaults).
func Load() Config {
	return Config{
		LogLevel:      envOr("LOG_LEVEL", "info"),
		RestAddr:      envOr("REST_ADDR", ":8080"),
		PluginSecrets: map[string]string{},
	}
}

// SecretFor liefert das Secret einer Plugin-ID (ENV: <ID>_SECRET, "-" -> "_").
func (c Config) SecretFor(pluginID string) string {
	key := ""
	for _, r := range pluginID {
		if r == '-' {
			key += "_"
		} else if r >= 'a' && r <= 'z' {
			key += string(r - 'a' + 'A')
		} else {
			key += string(r)
		}
	}
	if v, ok := c.PluginSecrets[key+"_SECRET"]; ok {
		return v
	}
	return os.Getenv(key + "_SECRET")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
