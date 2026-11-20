package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/settings"
)

// RequestPublicURL reconstructs the address the browser used: the scheme comes
// from X-Forwarded-Proto or a direct TLS connection, the host from
// X-Forwarded-Host or Host.
func RequestPublicURL(r *http.Request) string {
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

// PublicURLFor resolves the address OAuth callbacks and secure cookies must use.
// A configured process keeps its startup value; an instance that upgraded from
// the retired environment variable has nothing until the setup stores
// public_url, so the stored row comes next and the request address is the last
// resort. Without all three the caller would build a relative redirect_uri,
// which GitHub rejects.
func (a *Auth) PublicURLFor(ctx context.Context, r *http.Request) string {
	if configured := strings.TrimRight(strings.TrimSpace(a.PublicURL), "/"); configured != "" {
		return configured
	}
	if stored := a.storedPublicURL(ctx); stored != "" {
		return stored
	}
	detected := RequestPublicURL(r)
	if detected != "" {
		slog.Warn("public_url is not configured; guessing it from the request", "detected", detected)
	}
	return detected
}

func (a *Auth) storedPublicURL(ctx context.Context) string {
	if a.Store == nil {
		return ""
	}
	values, err := a.Store.Settings(ctx)
	if err != nil {
		slog.Warn("could not read public_url from settings", "error", err)
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(values[settings.KeyPublicURL]), "/")
}

func (a *Auth) secureCookies(publicURL string) bool {
	u, err := url.Parse(publicURL)
	return err == nil && strings.EqualFold(u.Scheme, "https")
}

// callbackURL is the redirect_uri both the authorize and the token exchange
// steps must send: GitHub compares them byte for byte.
func callbackURL(publicURL string) string {
	return strings.TrimRight(publicURL, "/") + "/auth/callback"
}
