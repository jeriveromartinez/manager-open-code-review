package github_test

import (
	"encoding/json"
	"reflect"
	"strings"
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
		"pull_request_review_thread",
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
	if m.HookAttributes == nil {
		t.Fatal("HookAttributes = nil with webhooks enabled, want non-nil")
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
	if !m.Public {
		t.Fatalf("Public = false, want true (multi-cuenta requiere App pública)")
	}
	raw, _ := json.Marshal(m)
	if !strings.Contains(string(raw), `"public":true`) {
		t.Fatalf("manifest JSON no declara public=true: %s", raw)
	}
	if !reflect.DeepEqual(m.DefaultPermissions, wantPermissions) {
		t.Errorf("DefaultPermissions = %v, want %v", m.DefaultPermissions, wantPermissions)
	}
	if !reflect.DeepEqual(m.DefaultEvents, wantEvents) {
		t.Errorf("DefaultEvents = %q, want %q", m.DefaultEvents, wantEvents)
	}

	disabled := ghapp.BuildManifest(publicURL, false, "qa-control-center")
	if disabled.HookAttributes != nil {
		t.Errorf("HookAttributes = %+v with webhooks disabled, want nil", disabled.HookAttributes)
	}
	data, err := json.Marshal(disabled)
	if err != nil {
		t.Fatalf("json.Marshal(disabled): %v", err)
	}
	if strings.Contains(string(data), `"hook_attributes"`) {
		t.Errorf("marshaled manifest contains hook_attributes with webhooks disabled: %s", data)
	}
}
