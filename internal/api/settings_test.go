package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
)

// The base fixture carries a hand-built config; the settings endpoints report
// what the process would really load, so these tests start from the defaults.
func newSettingsFixture(t *testing.T) *secureFixture {
	t.Helper()
	f := newSecureFixture(t)
	f.deps.Cfg = config.Default()
	f.rebuildRouter()
	return f
}

func TestSettingsRequireAdmin(t *testing.T) {
	f := newSettingsFixture(t)
	lead := f.user("lead", false)
	cookie := f.session(lead)
	assertStatus(t, f.request(http.MethodGet, "/api/settings", nil, cookie), http.StatusForbidden)
	assertStatus(t, f.request(http.MethodPut, "/api/settings", map[string]any{"values": map[string]string{"ocr_bin": "x"}}, cookie), http.StatusForbidden)
	assertStatus(t, f.request(http.MethodGet, "/api/settings", nil, nil), http.StatusUnauthorized)
}

func TestSettingsReturnsDefaultsAndMetadata(t *testing.T) {
	f := newSettingsFixture(t)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodGet, "/api/settings", nil, cookie)
	assertStatus(t, rec, http.StatusOK)

	var body struct {
		Values map[string]string `json:"values"`
		Fields []struct {
			Key     string `json:"key"`
			Group   string `json:"group"`
			Restart string `json:"restart"`
		} `json:"fields"`
	}
	decodeJSON(t, rec, &body)
	if body.Values["review_timeout"] != "20m0s" && body.Values["review_timeout"] != "20m" {
		t.Errorf("review_timeout = %q, want the 20m default", body.Values["review_timeout"])
	}
	if len(body.Fields) != 23 {
		t.Fatalf("fields = %d, want 23", len(body.Fields))
	}
}

func TestSettingsPutPersistsAndRejectsBadValues(t *testing.T) {
	f := newSettingsFixture(t)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"ocr_bin": "/usr/local/bin/ocr", "review_concurrency": "3"},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)

	stored, err := f.st.Settings(f.ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if stored["ocr_bin"] != "/usr/local/bin/ocr" || stored["review_concurrency"] != "3" {
		t.Errorf("stored = %v", stored)
	}
	if _, ok := stored["opencode_bin"]; ok {
		t.Error("a partial save must not write untouched keys")
	}

	rec = f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"review_timeout": "0s"},
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
	var apiErr map[string]string
	decodeJSON(t, rec, &apiErr)
	if !strings.Contains(apiErr["error"], "review_timeout") {
		t.Errorf("error = %q, want the offending key", apiErr["error"])
	}
}
