package api

import (
	"net/http"
	"strings"
)

// requestPublicURL reconstructs the address the browser used. public_url is
// empty until the instance is configured, and an existing installation that
// drops it from .env must still get a working callback URL.
func (s *server) requestPublicURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := firstHeaderValue(r, "X-Forwarded-Proto"); proto == "http" || proto == "https" {
		scheme = proto
	}
	host := strings.TrimSpace(r.Host)
	if forwarded := firstHeaderValue(r, "X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + strings.TrimRight(host, "/")
}

func firstHeaderValue(r *http.Request, name string) string {
	raw := r.Header.Get(name)
	if raw == "" {
		return ""
	}
	if comma := strings.IndexByte(raw, ','); comma >= 0 {
		raw = raw[:comma]
	}
	return strings.TrimSpace(raw)
}
