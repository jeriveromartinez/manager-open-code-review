package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
)

var (
	// ErrNoReviewConfig is returned when no review configuration applies to a
	// pull request.
	ErrNoReviewConfig = errors.New("no review configuration")
	// ErrProfileInUse is returned when deleting an LLM profile still referenced
	// by a team's QA policy.
	ErrProfileInUse = errors.New("llm profile in use")
)

// CreateLLMProfile inserts a profile whose APIKeyEnc is already encrypted with
// the worker key. The store never decrypts secrets.
func (s *Store) CreateLLMProfile(ctx context.Context, p LLMProfile) (LLMProfile, error) {
	if p.HealthStatus == "" {
		p.HealthStatus = HealthUnknown
	}
	if err := s.DB.WithContext(ctx).Create(&p).Error; err != nil {
		return LLMProfile{}, fmt.Errorf("create llm profile %q: %w", p.Name, err)
	}
	return p, nil
}

// UpdateLLMProfile replaces the editable fields of a profile, leaving its
// health state and timestamps alone. APIKeyEnc is the worker-sealed value.
func (s *Store) UpdateLLMProfile(ctx context.Context, p LLMProfile) error {
	res := s.DB.WithContext(ctx).Model(&LLMProfile{}).Where("id = ?", p.ID).Updates(map[string]any{
		"name":        p.Name,
		"provider":    p.Provider,
		"model":       p.Model,
		"variant":     p.Variant,
		"mode":        p.Mode,
		"api_key_enc": p.APIKeyEnc,
		"base_url":    p.BaseURL,
		"concurrency": p.Concurrency,
		"enabled":     p.Enabled,
		"updated_at":  time.Now().UTC(),
	})
	if res.Error != nil {
		return fmt.Errorf("update llm profile %d: %w", p.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update llm profile %d: %w", p.ID, ErrNotFound)
	}
	return nil
}

// DeleteLLMProfile removes a profile unless a QA policy still references it.
// The check and the delete run in one transaction so a policy cannot be added
// between them.
func (s *Store) DeleteLLMProfile(ctx context.Context, id uint) error {
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var policies int64
		if err := tx.Model(&QAPolicy{}).Where("llm_profile_id = ?", id).Count(&policies).Error; err != nil {
			return fmt.Errorf("count qa policies: %w", err)
		}
		if policies > 0 {
			return ErrProfileInUse
		}

		res := tx.Where("id = ?", id).Delete(&LLMProfile{})
		if res.Error != nil {
			return fmt.Errorf("delete row: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete llm profile %d: %w", id, err)
	}
	return nil
}

// ListLLMProfiles returns all profiles ordered by name.
func (s *Store) ListLLMProfiles(ctx context.Context) ([]LLMProfile, error) {
	var profiles []LLMProfile
	if err := s.DB.WithContext(ctx).Order("name").Find(&profiles).Error; err != nil {
		return nil, fmt.Errorf("list llm profiles: %w", err)
	}
	return profiles, nil
}

// LLMProfile loads a profile by ID, including its encrypted API key.
func (s *Store) LLMProfile(ctx context.Context, id uint) (LLMProfile, error) {
	var profile LLMProfile
	err := s.DB.WithContext(ctx).Take(&profile, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return LLMProfile{}, fmt.Errorf("llm profile %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return LLMProfile{}, fmt.Errorf("find llm profile %d: %w", id, err)
	}
	return profile, nil
}

// SetLLMHealth records the outcome of a provider health check.
func (s *Store) SetLLMHealth(ctx context.Context, id uint, status, message string) error {
	res := s.DB.WithContext(ctx).Model(&LLMProfile{}).Where("id = ?", id).Updates(map[string]any{
		"health_status":     status,
		"health_checked_at": time.Now().UTC(),
		"health_error":      message,
	})
	if res.Error != nil {
		return fmt.Errorf("set llm profile %d health: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("set llm profile %d health: %w", id, ErrNotFound)
	}
	return nil
}

// LockProfile takes a session-level PostgreSQL advisory lock keyed by profile
// ID on a dedicated pool connection. Call unlock when the critical section is
// done; it is safe to call more than once.
func (s *Store) LockProfile(ctx context.Context, profileID uint) (unlock func(), err error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for llm profile %d lock: %w", profileID, err)
	}

	key := int64(profileID)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		conn.Release()
		return nil, fmt.Errorf("lock llm profile %d: %w", profileID, err)
	}

	var once sync.Once
	unlock = func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Closing the connection releases the session lock even when the
			// explicit unlock fails, so no error can leak into the func() API.
			_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", key)
			conn.Release()
		})
	}
	return unlock, nil
}
