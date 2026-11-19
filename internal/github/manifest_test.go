package github_test

import (
	"reflect"
	"testing"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
)

func TestBuildManifest(t *testing.T) {
	const publicURL = "https://qa.example.com"

	wantPermissions := map[string]string{
		"pull_requests": "write",
		"checks":        "write",
		"contents":      "read",
		"metadata":      "read",
		"statuses":      "read",
		"members":       "read",
		"issues":        "write",
	}
	wantEvents := []string{
		"pull_request",
		"pull_request_review_comment",
		"issue_comment",
		"check_suite",
		"check_run",
		"status",
	}

	m := ghapp.BuildManifest(publicURL, true, "qa-control-center")

	if m.Name != "qa-control-center" {
		t.Errorf("Name = %q, want %q", m.Name, "qa-control-center")
	}
	if m.URL != publicURL {
		t.Errorf("URL = %q, want %q", m.URL, publicURL)
	}
	if m.HookAttributes.URL != publicURL+"/webhooks/github" {
		t.Errorf("HookAttributes.URL = %q, want %q", m.HookAttributes.URL, publicURL+"/webhooks/github")
	}
	if !m.HookAttributes.Active {
		t.Error("HookAttributes.Active = false, want true")
	}
	if m.RedirectURL != publicURL+"/setup/callback" {
		t.Errorf("RedirectURL = %q, want %q", m.RedirectURL, publicURL+"/setup/callback")
	}
	if want := []string{publicURL + "/auth/callback"}; !reflect.DeepEqual(m.CallbackURLs, want) {
		t.Errorf("CallbackURLs = %q, want %q", m.CallbackURLs, want)
	}
	if m.SetupURL != publicURL+"/setup/installed" {
		t.Errorf("SetupURL = %q, want %q", m.SetupURL, publicURL+"/setup/installed")
	}
	if m.Public {
		t.Error("Public = true, want false")
	}
	if !reflect.DeepEqual(m.DefaultPermissions, wantPermissions) {
		t.Errorf("DefaultPermissions = %v, want %v", m.DefaultPermissions, wantPermissions)
	}
	if !reflect.DeepEqual(m.DefaultEvents, wantEvents) {
		t.Errorf("DefaultEvents = %q, want %q", m.DefaultEvents, wantEvents)
	}

	disabled := ghapp.BuildManifest(publicURL, false, "qa-control-center")
	if disabled.HookAttributes.Active {
		t.Error("HookAttributes.Active = true with webhooks disabled, want false")
	}
	if disabled.HookAttributes.URL != m.HookAttributes.URL {
		t.Errorf("HookAttributes.URL changed with webhooks disabled: %q, want %q", disabled.HookAttributes.URL, m.HookAttributes.URL)
	}
}
