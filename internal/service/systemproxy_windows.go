//go:build windows

package service

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// detectSystemProxy reads the current user's Windows proxy
// setting (Internet Settings) — an explicit, user-visible OS
// configuration. Environment variables are never consulted.
// Per-protocol values ("http=host:port;https=host:port") prefer
// the https entry, then http, then a bare "host:port" value.
// The result is a URL string for http.ProxyURL; the fetcher
// still validates the endpoint before using it.
func detectSystemProxy() (string, bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer func() { _ = key.Close() }()
	enabled, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil || enabled == 0 {
		return "", false
	}
	server, _, err := key.GetStringValue("ProxyServer")
	if err != nil || strings.TrimSpace(server) == "" {
		return "", false
	}
	server = strings.TrimSpace(server)
	if strings.Contains(server, "=") {
		var httpVal, httpsVal string
		for _, part := range strings.Split(server, ";") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch strings.ToLower(kv[0]) {
			case "https":
				httpsVal = kv[1]
			case "http":
				httpVal = kv[1]
			}
		}
		server = httpsVal
		if server == "" {
			server = httpVal
		}
		if server == "" {
			return "", false
		}
	}
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	return server, true
}
