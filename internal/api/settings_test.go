package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

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
	if len(body.Fields) != 24 {
		t.Fatalf("fields = %d, want 24", len(body.Fields))
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

	rec = f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"review_timeout": "  30m  "},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	stored, err = f.st.Settings(f.ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if stored["review_timeout"] != "30m0s" {
		t.Errorf("stored review_timeout = %q, want the normalised 30m0s", stored["review_timeout"])
	}
}

// A save must be validated against the configuration the next start will load,
// not against s.Cfg, which stays at its startup value until the announced
// restart. Validating against s.Cfg lets these two saves persist a pair that
// aborts the next boot.
func TestSettingsPutValidatesAgainstStoredConfig(t *testing.T) {
	f := newSettingsFixture(t)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"review_timeout": "40m"},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)

	rec = f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"review_stale_after": "30m"},
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
	var apiErr map[string]string
	decodeJSON(t, rec, &apiErr)
	if !strings.Contains(apiErr["error"], "review_stale_after") {
		t.Errorf("error = %q, want the offending key", apiErr["error"])
	}

	stored, err := f.st.Settings(f.ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if _, ok := stored["review_stale_after"]; ok {
		t.Errorf("stored = %v, want no review_stale_after row after a rejected save", stored)
	}
}

// A row that only SQL can produce must not be reported as a bad save: the
// administrator needs the key that is corrupt, and the stored value itself must
// never travel back in the response.
func TestSettingsPutNamesCorruptRowWithoutEchoingIt(t *testing.T) {
	f := newSettingsFixture(t)
	cookie := f.session(f.user("admin", true))

	if err := f.st.SaveSettings(f.ctx, nil, map[string]string{"review_timeout": "0s"}); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}

	rec := f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"ocr_bin": "/usr/local/bin/ocr"},
	}, cookie)
	assertStatus(t, rec, http.StatusInternalServerError)

	var apiErr map[string]string
	decodeJSON(t, rec, &apiErr)
	if !strings.Contains(apiErr["error"], "review_timeout") {
		t.Errorf("error = %q, want the corrupt key", apiErr["error"])
	}
	if body := rec.Body.String(); strings.Contains(body, "0s") {
		t.Errorf("body = %q, want the stored value left out", body)
	}
}

func TestSettingsPutRecordsUpdatedBy(t *testing.T) {
	f := newSettingsFixture(t)
	admin := f.user("admin", true)

	rec := f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"ocr_bin": "/usr/local/bin/ocr"},
	}, f.session(admin))
	assertStatus(t, rec, http.StatusOK)

	var row struct {
		UpdatedBy *uint `gorm:"column:updated_by"`
	}
	if err := f.st.DB.WithContext(f.ctx).Table("app_settings").
		Select("updated_by").Where("key = ?", "ocr_bin").Take(&row).Error; err != nil {
		t.Fatalf("read app_settings: %v", err)
	}
	if row.UpdatedBy == nil || *row.UpdatedBy != admin.ID {
		t.Errorf("updated_by = %v, want %d", row.UpdatedBy, admin.ID)
	}
}

func TestSettingsPutRejectsEmptyAndUnknownValues(t *testing.T) {
	f := newSettingsFixture(t)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{},
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)

	rec = f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"not_a_setting": "x"},
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
	var apiErr map[string]string
	decodeJSON(t, rec, &apiErr)
	if !strings.Contains(apiErr["error"], "not_a_setting") {
		t.Errorf("error = %q, want the unknown key", apiErr["error"])
	}
}

func TestSettingsPutRequiresAuthentication(t *testing.T) {
	f := newSettingsFixture(t)
	rec := f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"ocr_bin": "x"},
	}, nil)
	assertStatus(t, rec, http.StatusUnauthorized)
}

func TestSettingsGetOverlaysStoredRowsOverProcessConfig(t *testing.T) {
	f := newSettingsFixture(t)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"review_timeout": "30m"},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)

	rec = f.request(http.MethodGet, "/api/settings", nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var body struct {
		Values map[string]string `json:"values"`
	}
	decodeJSON(t, rec, &body)
	if body.Values["review_timeout"] != "30m0s" {
		t.Errorf("review_timeout = %q, want the stored 30m0s", body.Values["review_timeout"])
	}
	if f.deps.Cfg.ReviewTimeout != 20*time.Minute {
		t.Errorf("deps.Cfg.ReviewTimeout = %s, want the untouched 20m startup value", f.deps.Cfg.ReviewTimeout)
	}
}
