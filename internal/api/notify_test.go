package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

type failingPublisher struct{}

func (failingPublisher) Publish(context.Context, events.Event) error {
	return errors.New("notify unavailable")
}

func assertEvents(t *testing.T, got []events.Event, want ...events.Event) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("published events = %+v, want %+v", got, want)
	}
}

func idString(id uint) string {
	return strconv.FormatUint(uint64(id), 10)
}

// The verdict can change even when the finding status does not, so a repeated
// action still notifies the pull request.
func TestTriageNotifiesThePR(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	cookie := f.session(f.user("triage-notify", true))
	want := events.Event{Type: "pr.updated", ID: idString(pr.ID)}

	withdraw := fmt.Sprintf("/api/prs/%d/findings/%s/withdraw", pr.ID, finding.ID)
	confirm := fmt.Sprintf("/api/prs/%d/findings/%s/confirm", pr.ID, finding.ID)
	for _, path := range []string{withdraw, withdraw, confirm} {
		assertStatus(t, f.request(http.MethodPost, path, nil, cookie), http.StatusOK)
	}

	assertEvents(t, f.pub.snapshot(), want, want, want)
}

func TestTriageWithFailedNotificationStillSucceeds(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	f.deps.Events = failingPublisher{}
	f.rebuildRouter()
	cookie := f.session(f.user("triage-notify-fail", true))

	path := fmt.Sprintf("/api/prs/%d/findings/%s/withdraw", pr.ID, finding.ID)
	assertStatus(t, f.request(http.MethodPost, path, nil, cookie), http.StatusOK)

	var got store.Finding
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", finding.ID).Take(&got).Error; err != nil {
		t.Fatalf("reload finding: %v", err)
	}
	if got.Status != store.FindingWithdrawn {
		t.Errorf("status = %q, want %q", got.Status, store.FindingWithdrawn)
	}
}

func TestTeamWritesNotifyTheTeam(t *testing.T) {
	f := newSecureFixture(t)
	member := f.user("team-member", false)
	cookie := f.session(f.user("team-admin", true))
	profile := seedProfile(t, f, "team-notify", store.LLMProfile{Enabled: true, HealthStatus: store.HealthOK})
	// A team created straight in the store moves the team ids past the policy
	// ids, so an event carrying the policy id instead of the team id fails.
	if _, err := f.st.CreateTeam(f.ctx, "Decoy"); err != nil {
		t.Fatalf("create decoy team: %v", err)
	}

	rec := f.request(http.MethodPost, "/api/teams", map[string]any{"name": "Notified"}, cookie)
	assertStatus(t, rec, http.StatusCreated)
	var created struct {
		ID uint `json:"id"`
	}
	decodeJSON(t, rec, &created)
	want := events.Event{Type: "team.updated", ID: idString(created.ID)}
	assertEvents(t, f.pub.snapshot(), want)

	memberPath := fmt.Sprintf("/api/teams/%d/members/%d", created.ID, member.ID)
	assertStatus(t, f.request(http.MethodPut, memberPath, map[string]any{"role": "qa"}, cookie), http.StatusNoContent)
	assertEvents(t, f.pub.snapshot(), want, want)

	assertStatus(t, f.request(http.MethodDelete, memberPath, nil, cookie), http.StatusNoContent)
	assertEvents(t, f.pub.snapshot(), want, want, want)

	body := policyBodyFor(profile.ID)
	body["confirm_unhealthy_profile"] = true
	policyPath := fmt.Sprintf("/api/teams/%d/policy", created.ID)
	assertStatus(t, f.request(http.MethodPut, policyPath, body, cookie), http.StatusOK)
	assertEvents(t, f.pub.snapshot(), want, want, want, want)
}

func TestLLMProfileWritesNotifyTheProfile(t *testing.T) {
	f := newSecureFixture(t)
	cookie := f.session(f.user("profile-admin", true))

	rec := f.request(http.MethodPost, "/api/llm-profiles", profileBody(), cookie)
	assertStatus(t, rec, http.StatusCreated)
	var created struct {
		ID uint `json:"id"`
	}
	decodeJSON(t, rec, &created)
	want := events.Event{Type: "llm_profile.updated", ID: idString(created.ID)}
	assertEvents(t, f.pub.snapshot(), want)

	path := fmt.Sprintf("/api/llm-profiles/%d", created.ID)
	assertStatus(t, f.request(http.MethodPatch, path, map[string]any{"name": "renamed"}, cookie), http.StatusOK)
	assertEvents(t, f.pub.snapshot(), want, want)

	assertStatus(t, f.request(http.MethodPost, path+"/health", nil, cookie), http.StatusAccepted)
	assertEvents(t, f.pub.snapshot(), want, want, want)

	assertStatus(t, f.request(http.MethodDelete, path, nil, cookie), http.StatusNoContent)
	assertEvents(t, f.pub.snapshot(), want, want, want, want)
}

func TestSettingsSaveNotifies(t *testing.T) {
	f := newSettingsFixture(t)
	cookie := f.session(f.user("settings-admin", true))
	want := events.Event{Type: "settings.updated", ID: ""}

	rec := f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"ocr_bin": "/usr/local/bin/ocr"},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)
	assertEvents(t, f.pub.snapshot(), want)

	rec = f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"review_timeout": "0s"},
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
	assertEvents(t, f.pub.snapshot(), want)
}
