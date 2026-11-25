package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Objection actions are the explicit decisions a reevaluation of a reply on a
// finding's thread can reach. They are audit values, not finding statuses.
const (
	ObjectionUphold    = "uphold"
	ObjectionWithdraw  = "withdraw"
	ObjectionDowngrade = "downgrade"
	ObjectionEscalate  = "escalate"
	ObjectionRecorded  = "recorded"
	ObjectionIgnored   = "ignored"
)

// Finding objection states. A queued intent is evaluated by the worker, an
// evaluated one is waiting for the publisher's reply, and a published one is
// final.
const (
	ObjectionQueued    = "queued"
	ObjectionEvaluated = "evaluated"
	ObjectionPublished = "published"
	// ObjectionFailed is terminal: the reply could not be published after the
	// retry budget and a human must take over. It is never re-enqueued.
	ObjectionFailed = "failed"
)

// EnqueueFindingObjection records the reply that disputes a finding and reports
// whether the intent is new. A human triage or a resolved finding is rejected
// before the row exists. The unique (finding, comment) key makes a redelivery
// the same objection.
func (s *Store) EnqueueFindingObjection(ctx context.Context, in FindingObjection) (FindingObjection, bool, error) {
	created := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Finding
		err := quietDB(tx).Where("id = ?", in.FindingID).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("enqueue objection of finding %q: %w", in.FindingID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("find finding %q: %w", in.FindingID, err)
		}
		if current.Status == FindingResolved {
			return fmt.Errorf("enqueue objection of finding %q: %w", in.FindingID, ErrFindingResolved)
		}
		if current.TriageSource == TriageSourceHuman {
			return fmt.Errorf("enqueue objection of finding %q: %w", in.FindingID, ErrFindingHumanTriaged)
		}

		now := time.Now().UTC()
		readToken, fileContent := in.ReadTokenEnc, in.FileContent
		in.ID = 0
		in.State = ObjectionQueued
		in.Action, in.Status, in.Apply = "", "", false
		in.Reason, in.Evidence, in.Verification = "", "", nil
		in.RepliedAt = nil
		in.CreatedAt, in.UpdatedAt = now, now
		res := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "finding_id"}, {Name: "comment_id"}},
			DoNothing: true,
		}).Create(&in)
		if res.Error != nil {
			return fmt.Errorf("insert objection of finding %q: %w", in.FindingID, res.Error)
		}
		if res.RowsAffected > 0 {
			created = true
			return nil
		}
		if err := quietDB(tx).
			Where("finding_id = ? AND comment_id = ?", in.FindingID, in.CommentID).
			Take(&in).Error; err != nil {
			return fmt.Errorf("load objection of finding %q: %w", in.FindingID, err)
		}
		// A redelivery may carry a fresher read token or the file the app had
		// already fetched; refresh them while the intent still waits for the
		// worker.
		if in.State != ObjectionQueued {
			return nil
		}
		updates := map[string]any{}
		if readToken != "" && readToken != in.ReadTokenEnc {
			updates["read_token_enc"] = readToken
			in.ReadTokenEnc = readToken
		}
		if len(fileContent) > 0 {
			updates["file_content"] = fileContent
			in.FileContent = fileContent
		}
		if len(updates) == 0 {
			return nil
		}
		updates["updated_at"] = now
		if err := tx.Model(&FindingObjection{}).Where("id = ?", in.ID).Updates(updates).Error; err != nil {
			return fmt.Errorf("refresh objection of finding %q: %w", in.FindingID, err)
		}
		return nil
	})
	if err != nil {
		return FindingObjection{}, false, err
	}
	return in, created, nil
}

// FindingObjection loads one objection intent by id.
func (s *Store) FindingObjection(ctx context.Context, id uint) (FindingObjection, error) {
	var out FindingObjection
	err := quietDB(s.DB.WithContext(ctx)).Take(&out, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return FindingObjection{}, fmt.Errorf("finding objection %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return FindingObjection{}, fmt.Errorf("load finding objection %d: %w", id, err)
	}
	return out, nil
}

// ObjectionEvaluation is the bounded reevaluation's outcome for one intent.
type ObjectionEvaluation struct {
	Action       string
	Status       string
	Apply        bool
	Reason       string
	Evidence     string
	Verification []byte
}

// RecordFindingObjectionEvaluation applies a reevaluation atomically: the
// finding status, its verification and the audit row move together with the
// intent's evaluated state. Repeating an evaluation is a no-op. A human triage
// or a resolved finding wins even if it landed after the intent was queued.
func (s *Store) RecordFindingObjectionEvaluation(ctx context.Context, id uint, eval ObjectionEvaluation) (FindingObjection, error) {
	var out FindingObjection
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := quietDB(tx).Where("id = ?", id).
			Clauses(clause.Locking{Strength: "UPDATE"}).Take(&out).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("record objection %d: %w", id, ErrNotFound)
			}
			return fmt.Errorf("find objection %d: %w", id, err)
		}
		if out.State != ObjectionQueued {
			return nil
		}

		var current Finding
		if err := quietDB(tx).Where("id = ?", out.FindingID).
			Clauses(clause.Locking{Strength: "UPDATE"}).Take(&current).Error; err != nil {
			return fmt.Errorf("find finding %q: %w", out.FindingID, err)
		}

		now := time.Now().UTC()
		action := eval.Action
		status := eval.Status
		apply := eval.Apply
		reason := eval.Reason
		evidence := eval.Evidence

		// A decision made after the intent was queued is final. The worker must
		// not revive a finding a person already triaged or the code fixed.
		if current.Status == FindingResolved || current.TriageSource == TriageSourceHuman {
			action = ObjectionIgnored
			status, apply = "", false
			reason = "la decisión humana no se reevalúa"
			evidence = ""
		} else if out.Authorized && apply && status != "" && current.Status != status {
			updates := map[string]any{
				"status":                    status,
				"triage_verdict":            verdictForStatus(status),
				"triaged_by":                out.Actor,
				"triaged_at":                now,
				"triage_source":             TriageSourceVerifier,
				"github_thread_decision_at": now,
				"updated_at":                now,
			}
			if out.Authorized && len(eval.Verification) > 0 {
				updates["verification"] = datatypes.JSON(eval.Verification)
			}
			if err := tx.Model(&Finding{}).Where("id = ?", out.FindingID).Updates(updates).Error; err != nil {
				return fmt.Errorf("apply objection to finding %q: %w", out.FindingID, err)
			}
		} else if out.Authorized && len(eval.Verification) > 0 {
			if err := tx.Model(&Finding{}).Where("id = ?", out.FindingID).Updates(map[string]any{
				"verification": datatypes.JSON(eval.Verification),
				"updated_at":   now,
			}).Error; err != nil {
				return fmt.Errorf("record objection verification of finding %q: %w", out.FindingID, err)
			}
		}

		entry := FindingAudit{
			Action:     objectionAuditAction(action),
			Actor:      out.Actor,
			ActorID:    out.ActorID,
			CommentID:  out.CommentID,
			Reason:     reason,
			DeliveryID: out.DeliveryID,
			Evidence:   evidence,
			HeadSHA:    out.HeadSHA,
			Objection:  out.Body,
		}
		if out.Authorized && action != ObjectionIgnored {
			entry.Source = TriageSourceVerifier
		}
		if err := writeFindingAudit(tx, out.FindingID, entry); err != nil {
			return err
		}

		updates := map[string]any{
			"state":        ObjectionEvaluated,
			"action":       action,
			"status":       status,
			"apply":        apply,
			"reason":       reason,
			"evidence":     evidence,
			"updated_at":   now,
			"verification": nil,
		}
		if out.Authorized && len(eval.Verification) > 0 {
			updates["verification"] = datatypes.JSON(eval.Verification)
		}
		if err := tx.Model(&FindingObjection{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return fmt.Errorf("record objection %d evaluation: %w", id, err)
		}
		out.State = ObjectionEvaluated
		out.Action, out.Status, out.Apply = action, status, apply
		out.Reason, out.Evidence = reason, evidence
		if out.Authorized {
			out.Verification = datatypes.JSON(eval.Verification)
		}
		out.UpdatedAt = now
		return nil
	})
	if err != nil {
		return FindingObjection{}, err
	}
	return out, nil
}

// MarkFindingObjectionPublished closes an evaluated intent. Repeating it is a
// no-op, so a retried publish cannot reopen the work.
func (s *Store) MarkFindingObjectionPublished(ctx context.Context, id uint) error {
	res := s.DB.WithContext(ctx).Model(&FindingObjection{}).
		Where("id = ? AND state = ?", id, ObjectionEvaluated).
		Updates(map[string]any{
			"state":      ObjectionPublished,
			"replied_at": time.Now().UTC(),
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return fmt.Errorf("mark objection %d published: %w", id, res.Error)
	}
	return nil
}

// FailFindingObjectionPublication makes an evaluated intent terminal after the
// publisher exhausted its retries. The audit row keeps the cause, and
// PendingObjectionPublications no longer returns it, so the sweeper stops
// re-enqueueing a reply that cannot be posted.
func (s *Store) FailFindingObjectionPublication(ctx context.Context, id uint, failure string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var intent FindingObjection
		if err := quietDB(tx).Where("id = ?", id).
			Clauses(clause.Locking{Strength: "UPDATE"}).Take(&intent).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("fail objection %d: %w", id, ErrNotFound)
			}
			return fmt.Errorf("find objection %d: %w", id, err)
		}
		if intent.State != ObjectionEvaluated {
			return nil
		}
		now := time.Now().UTC()
		if err := tx.Model(&FindingObjection{}).Where("id = ?", id).Updates(map[string]any{
			"state":      ObjectionFailed,
			"failure":    failure,
			"updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("fail objection %d: %w", id, err)
		}
		return writeFindingAudit(tx, intent.FindingID, FindingAudit{
			Action:     "finding.objection.publish_failed",
			Actor:      intent.Actor,
			ActorID:    intent.ActorID,
			CommentID:  intent.CommentID,
			Reason:     failure,
			DeliveryID: intent.DeliveryID,
			Evidence:   intent.Evidence,
			HeadSHA:    intent.HeadSHA,
			Objection:  intent.Body,
		})
	})
}

// PendingObjectionEvaluations lists queued intents old enough to have lost
// their job, so the reconciler can enqueue them again.
func (s *Store) PendingObjectionEvaluations(ctx context.Context, before time.Time, limit int) ([]FindingObjection, error) {
	var out []FindingObjection
	err := s.DB.WithContext(ctx).
		Where("state = ? AND created_at <= ?", ObjectionQueued, before).
		Order("id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("list pending objection evaluations: %w", err)
	}
	return out, nil
}

// PendingObjectionPublications lists evaluated intents whose reply has not been
// confirmed yet.
func (s *Store) PendingObjectionPublications(ctx context.Context, limit int) ([]FindingObjection, error) {
	var out []FindingObjection
	err := s.DB.WithContext(ctx).
		Where("state = ?", ObjectionEvaluated).
		Order("id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("list pending objection publications: %w", err)
	}
	return out, nil
}

func objectionAuditAction(action string) string {
	if action == "" {
		action = ObjectionRecorded
	}
	return "finding.objection." + action
}

func verdictForStatus(status string) string {
	if status == FindingWithdrawn {
		return VerdictFalsePositive
	}
	return VerdictConfirmed
}
