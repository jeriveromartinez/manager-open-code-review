package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// PRValidations is the check state measured on one head SHA of a pull request.
// Passed counts successful and neutral checks, Failed counts the failing ones
// and Total is everything GitHub reported for the commit, so Passed+Failed
// never exceeds Total while in-flight checks make it lower.
type PRValidations struct {
	Passed int
	Failed int
	Total  int
}

// Green reports whether the head has at least one check and every check
// finished green. A head without checks, or with one still running, is not
// green: the CI gate must not open on an unknown verdict.
func (v PRValidations) Green() bool { return v.Total > 0 && v.Passed == v.Total }

// SetPRValidations stores the counters of headSHA without a named observation.
// It is the counters-only writer for rows created before the named observation
// existed; it shares the stamped path so it cannot overwrite a newer reading.
// A write for a head the pull request no longer points at is discarded, so a
// late delivery cannot label the new commit with the verdict of the old one.
// changed reports whether the stored state actually moved.
func (s *Store) SetPRValidations(ctx context.Context, prID uint, headSHA string, v PRValidations) (bool, error) {
	write, err := s.storeValidations(ctx, prID, headSHA, v, nil, ObservationStamp{
		At: time.Now().UTC(), Source: ObservationSourceLegacy,
	})
	return write.Changed, err
}

// PRValidationsFor returns the stored counters of headSHA. A head the pull
// request has moved past has no counters and is reported as zero.
func (s *Store) PRValidationsFor(ctx context.Context, prID uint, headSHA string) (PRValidations, error) {
	var pr PullRequest
	err := quietDB(s.DB.WithContext(ctx)).Select(
		"id", "head_sha", "validations_passed", "validations_failed", "validations_total",
	).Take(&pr, prID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PRValidations{}, fmt.Errorf("pull request %d: %w", prID, ErrNotFound)
	}
	if err != nil {
		return PRValidations{}, fmt.Errorf("find pull request %d: %w", prID, err)
	}
	if headSHA == "" || pr.HeadSHA != headSHA {
		return PRValidations{}, nil
	}
	return PRValidations{
		Passed: pr.ValidationsPassed,
		Failed: pr.ValidationsFailed,
		Total:  pr.ValidationsTotal,
	}, nil
}
