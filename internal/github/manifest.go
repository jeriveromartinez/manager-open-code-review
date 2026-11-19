// Package github implements the GitHub App integration: manifest creation,
// credential persistence and installation-scoped API clients.
package github

// Manifest is the JSON payload GitHub accepts when creating an App from a
// manifest.
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     HookAttributes    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	CallbackURLs       []string          `json:"callback_urls"`
	SetupURL           string            `json:"setup_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// HookAttributes configures the webhook installed along with the App.
type HookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// BuildManifest builds the manifest for an App hosted at publicURL. Webhook
// events are requested only when webhooksEnabled is true; installation events
// are always delivered by GitHub and are not listed explicitly.
func BuildManifest(publicURL string, webhooksEnabled bool, name string) Manifest {
	return Manifest{
		Name:           name,
		URL:            publicURL,
		HookAttributes: HookAttributes{URL: publicURL + "/webhooks/github", Active: webhooksEnabled},
		RedirectURL:    publicURL + "/setup/callback",
		CallbackURLs:   []string{publicURL + "/auth/callback"},
		SetupURL:       publicURL + "/setup/installed",
		Public:         false,
		DefaultPermissions: map[string]string{
			"pull_requests": "write",
			"checks":        "write",
			"contents":      "read",
			"metadata":      "read",
			"statuses":      "read",
			"members":       "read",
			"issues":        "write",
		},
		DefaultEvents: []string{
			"pull_request",
			"pull_request_review_comment",
			"issue_comment",
			"check_suite",
			"check_run",
			"status",
		},
	}
}
