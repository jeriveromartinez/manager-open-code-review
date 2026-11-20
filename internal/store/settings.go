package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

type settingRow struct {
	Key       string    `gorm:"column:key;primaryKey"`
	Value     string    `gorm:"column:value;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
	UpdatedBy *uint     `gorm:"column:updated_by"`
}

func (settingRow) TableName() string { return "app_settings" }

// Settings returns the stored overrides. Keys absent from the map still use
// their defaults from internal/settings, so an empty table is a valid state.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	var rows []settingRow
	if err := s.DB.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list app settings: %w", err)
	}
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		values[row.Key] = row.Value
	}
	return values, nil
}

// SaveSettings upserts only the given keys, so two administrators editing
// different sections do not overwrite each other.
func (s *Store) SaveSettings(ctx context.Context, userID *uint, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	rows := make([]settingRow, 0, len(values))
	now := time.Now().UTC()
	for key, value := range values {
		rows = append(rows, settingRow{Key: key, Value: value, UpdatedAt: now, UpdatedBy: userID})
	}
	err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at", "updated_by"}),
	}).Create(&rows).Error
	if err != nil {
		return fmt.Errorf("save app settings: %w", err)
	}
	return nil
}
