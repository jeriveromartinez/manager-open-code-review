package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrHumanVerdictNotApplicable reports that a human verdict cannot be recorded
// for the pull request in its current state (closed, draft or without a head).
var ErrHumanVerdictNotApplicable = errors.New("pull request is not open for a human verdict")

// HumanVerdictScopeOf decodes the stored scope. A malformed value yields an
// empty scope, which never counts as complete evidence.
func HumanVerdictScopeOf(v HumanVerdict) HumanVerdictScope {
	var scope HumanVerdictScope
	if len(v.Scope) > 0 {
		_ = json.Unmarshal(v.Scope, &scope)
	}
	return scope
}

// EncodeHumanVerdictScope validates and encodes the scope of a human verdict.
func EncodeHumanVerdictScope(scope HumanVerdictScope) (datatypes.JSON, error) {
	for i := range scope.Files {
		scope.Files[i].Path = strings.TrimSpace(scope.Files[i].Path)
		if scope.Files[i].Path == "" {
			return nil, errors.New("human verdict scope file path is empty")
		}
		if scope.Files[i].StartLine < 0 || scope.Files[i].EndLine < 0 {
			return nil, fmt.Errorf("human verdict scope %q has a negative line", scope.Files[i].Path)
		}
		if scope.Files[i].EndLine > 0 && scope.Files[i].EndLine < scope.Files[i].StartLine {
			return nil, fmt.Errorf("human verdict scope %q ends before it starts", scope.Files[i].Path)
		}
	}
	pending := make([]string, 0, len(scope.PendingFindings))
	for _, id := range scope.PendingFindings {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errors.New("human verdict scope has an empty pending finding id")
		}
		pending = append(pending, id)
	}
	scope.PendingFindings = pending
	if !scope.Complete && len(scope.Files) == 0 && len(scope.PendingFindings) == 0 {
		return nil, errors.New("human verdict must name the scope it reviewed or declare it complete")
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return nil, fmt.Errorf("encode human verdict scope: %w", err)
	}
	return datatypes.JSON(raw), nil
}

// RecordHumanVerdict stores a person's decision about an inconclusive review
// and invalidates the previous active one in the same transaction. The
// comparison is always the pull request's current one, never a caller-supplied
// snapshot, so a stale decision cannot be recorded after a push or retarget.
func (s *Store) RecordHumanVerdict(ctx context.Context, in HumanVerdict) (HumanVerdict, error) {
	if in.Decision != HumanVerdictApproved && in.Decision != HumanVerdictChangesRequested {
		return HumanVerdict{}, fmt.Errorf("human verdict decision %q is not supported", in.Decision)
	}
	scope := HumanVerdictScopeOf(in)
	encoded, err := EncodeHumanVerdictScope(scope)
	if err != nil {
		return HumanVerdict{}, err
	}
	in.Scope = encoded
	in.ScopeComplete = scope.Complete
	if in.Source == "" {
		in.Source = HumanVerdictSourcePanel
	}

	var saved HumanVerdict
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var pr PullRequest
		err := quietDB(tx).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&pr, in.PullRequestID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("human verdict for pull request %d: %w", in.PullRequestID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("lock pull request %d: %w", in.PullRequestID, err)
		}
		if pr.State != stateOpen || pr.Draft {
			return fmt.Errorf("human verdict for pull request %d: %w", pr.ID, ErrHumanVerdictNotApplicable)
		}
		if pr.HeadSHA == "" {
			return fmt.Errorf("human verdict for pull request %d: %w", pr.ID, ErrHumanVerdictNotApplicable)
		}
		if _, err := invalidateHumanVerdicts(tx, pr.ID, HumanVerdictInvalidatedReplaced); err != nil {
			return err
		}

		now := time.Now().UTC()
		row := in
		row.ID = 0
		row.PullRequestID = pr.ID
		row.HeadSHA = pr.HeadSHA
		row.BaseRef = pr.BaseRef
		row.MergeBaseSHA = pr.MergeBaseSHA
		row.InvalidatedAt = nil
		row.InvalidatedReason = ""
		row.CreatedAt = now
		row.UpdatedAt = now
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("record human verdict for pull request %d: %w", pr.ID, err)
		}
		saved = row
		return nil
	})
	if err != nil {
		return HumanVerdict{}, err
	}
	return saved, nil
}

// CurrentHumanVerdict returns the active verdict that still describes the pull
// request's current comparison. A verdict of another head, base branch or merge
// base is history and is never applied.
func (s *Store) CurrentHumanVerdict(ctx context.Context, pr PullRequest) (HumanVerdict, bool, error) {
	var rows []HumanVerdict
	err := s.DB.WithContext(ctx).
		Where("pull_request_id = ? AND invalidated_at IS NULL", pr.ID).
		Order("id DESC").
		Find(&rows).Error
	if err != nil {
		return HumanVerdict{}, false, fmt.Errorf("list human verdicts of pull request %d: %w", pr.ID, err)
	}
	for _, row := range rows {
		if row.Covers(pr) {
			return row, true, nil
		}
	}
	return HumanVerdict{}, false, nil
}

// LatestHumanVerdict returns the most recent verdict of a pull request,
// including an invalidated one, so the panel can explain what changed and why
// the decision no longer applies. It returns false when no verdict was ever
// recorded.
func (s *Store) LatestHumanVerdict(ctx context.Context, prID uint) (HumanVerdict, bool, error) {
	var row HumanVerdict
	err := quietDB(s.DB.WithContext(ctx)).
		Where("pull_request_id = ?", prID).
		Order("id DESC").
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return HumanVerdict{}, false, nil
	}
	if err != nil {
		return HumanVerdict{}, false, fmt.Errorf("find latest human verdict of pull request %d: %w", prID, err)
	}
	return row, true, nil
}

// invalidateHumanVerdicts marks every active verdict of a pull request as
// history. It runs inside the transaction that moved the comparison or recorded
// a replacement.
func invalidateHumanVerdicts(tx *gorm.DB, prID uint, reason string) (int64, error) {
	if reason == "" {
		reason = HumanVerdictInvalidatedReplaced
	}
	now := time.Now().UTC()
	res := tx.Model(&HumanVerdict{}).
		Where("pull_request_id = ? AND invalidated_at IS NULL", prID).
		Updates(map[string]any{
			"invalidated_at":     now,
			"invalidated_reason": reason,
			"updated_at":         now,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("invalidate human verdicts of pull request %d: %w", prID, res.Error)
	}
	return res.RowsAffected, nil
}
