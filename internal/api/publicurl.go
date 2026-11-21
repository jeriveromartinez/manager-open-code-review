package api

import (
	"net/http"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
)

// requestPublicURL reconstructs the address the browser used. public_url is
// empty until the instance is configured, and an existing installation that
// drops it from .env must still get a working callback URL. The detection lives
// in auth so the setup and the OAuth flow cannot drift apart.
func (s *server) requestPublicURL(r *http.Request) string {
	return auth.RequestPublicURL(r)
}
