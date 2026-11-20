package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// settingRow mirrors the store's unexported row so this external test can
// assert updated_by; testutil imports store, so an internal test cannot.
type settingRow struct {
	Key       string    `gorm:"column:key;primaryKey"`
	Value     string    `gorm:"column:value;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
	UpdatedBy *uint     `gorm:"column:updated_by"`
}

func (settingRow) TableName() string { return "app_settings" }

func TestSettingsRoundTrip(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()

	values, err := s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings on empty table: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("Settings on empty table = %v, want empty", values)
	}

	if err := s.SaveSettings(ctx, nil, map[string]string{
		"review_timeout": "25m",
		"ocr_bin":        "ocr-dev",
	}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	values, err = s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if values["review_timeout"] != "25m" || values["ocr_bin"] != "ocr-dev" {
		t.Errorf("Settings = %v, want review_timeout=25m ocr_bin=ocr-dev", values)
	}
}

func TestSaveSettingsIsPartialAndRecordsUser(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	user := &store.User{GitHubID: 9001, Login: "admin", IsAdmin: true}
	if err := s.DB.WithContext(ctx).Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := s.SaveSettings(ctx, nil, map[string]string{"ocr_bin": "ocr-a", "opencode_bin": "oc-a"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if err := s.SaveSettings(ctx, &user.ID, map[string]string{"ocr_bin": "ocr-b"}); err != nil {
		t.Fatalf("SaveSettings partial: %v", err)
	}

	values, err := s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if values["ocr_bin"] != "ocr-b" {
		t.Errorf("ocr_bin = %q, want ocr-b", values["ocr_bin"])
	}
	if values["opencode_bin"] != "oc-a" {
		t.Errorf("opencode_bin = %q, want oc-a: a partial write must keep the other keys", values["opencode_bin"])
	}

	var row settingRow
	if err := s.DB.WithContext(ctx).Where("key = ?", "ocr_bin").First(&row).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}
	if row.UpdatedBy == nil || *row.UpdatedBy != user.ID {
		t.Errorf("updated_by = %v, want %d", row.UpdatedBy, user.ID)
	}

	var opencodeRow settingRow
	if err := s.DB.WithContext(ctx).Where("key = ?", "opencode_bin").First(&opencodeRow).Error; err != nil {
		t.Fatalf("read opencode_bin row: %v", err)
	}
	if opencodeRow.UpdatedBy != nil {
		t.Errorf("opencode_bin updated_by = %d, want NULL", *opencodeRow.UpdatedBy)
	}
}
