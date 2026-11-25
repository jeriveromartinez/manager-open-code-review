package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Check states observed on a head. A check that has not completed yet is
// pending, never passed.
const (
	CheckPassed  = "passed"
	CheckFailed  = "failed"
	CheckPending = "pending"
)

// Observation results. A lookup failure is stored explicitly: the counters of
// the head are cleared so the gate cannot keep using a verdict GitHub did not
// confirm.
const (
	ObservationOK           = "ok"
	ObservationLookupFailed = "lookup_failed"
)

// Observation sources. They say which path read the head, which helps explain
// a stored verdict and keeps webhook, polling and publication readings apart.
const (
	ObservationSourceWebhook = "webhook"
	ObservationSourcePolling = "polling"
	ObservationSourcePublish = "publish"
	ObservationSourceUnknown = "unknown"
)

// ObservationStamp says when a head was read and by which path. Timestamps are
// taken before the GitHub lookup, so a slow read cannot claim to be newer than
// a delivery that arrived while it was in flight.
type ObservationStamp struct {
	At     time.Time
	Source string
}

// Gate states. Only Green opens the gate or publishes a success.
const (
	CIGreen   = "green"
	CIPending = "pending"
	CIFailed  = "failed"
	CIAbsent  = "absent"
	CIUnknown = "unknown"
)

// CheckObservation is one check run or commit status seen on a head. Origin
// tells where it came from: "app:<id>" for a check run of a known App, or
// "status" for a legacy commit status.
type CheckObservation struct {
	Name   string `json:"name"`
	Origin string `json:"origin,omitempty"`
	State  string `json:"state"`
}

// ValidationObservation is the named check state of one head. Result records
// whether GitHub actually answered the lookup.
type ValidationObservation struct {
	Result string             `json:"result"`
	Checks []CheckObservation `json:"checks,omitempty"`
}

// RequiredCheck names one check the gate needs. An empty Origin matches the
// name from any origin; otherwise both name and origin must match.
type RequiredCheck struct {
	Name   string `json:"name"`
	Origin string `json:"origin,omitempty"`
}

// CIVerdict is the single gate decision shared by enqueueing, publication and
// republication. Missing lists the required checks that were not observed.
type CIVerdict struct {
	State   string
	Reason  string
	Missing []string
}

// Green reports whether the gate may open.
func (v CIVerdict) Green() bool { return v.State == CIGreen }

// ParseRequiredChecks decodes the policy column. A missing or malformed value
// is an empty list, which keeps the previous "every reported check" rule.
func ParseRequiredChecks(raw datatypes.JSON) []RequiredCheck {
	if len(raw) == 0 {
		return nil
	}
	var checks []RequiredCheck
	if err := json.Unmarshal(raw, &checks); err != nil {
		return nil
	}
	out := make([]RequiredCheck, 0, len(checks))
	for _, check := range checks {
		check.Name = strings.TrimSpace(check.Name)
		check.Origin = strings.TrimSpace(check.Origin)
		if check.Name == "" || !validCheckOrigin(check.Origin) {
			continue
		}
		out = append(out, check)
	}
	return out
}

// RequiredChecks decodes the policy column and reports whether every entry is
// usable. An unreadable policy must close the gate, never fall back to a weaker
// rule.
func RequiredChecks(raw datatypes.JSON) ([]RequiredCheck, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, true
	}
	var checks []RequiredCheck
	if err := json.Unmarshal(raw, &checks); err != nil {
		return nil, false
	}
	out := make([]RequiredCheck, 0, len(checks))
	for _, check := range checks {
		check.Name = strings.TrimSpace(check.Name)
		check.Origin = strings.TrimSpace(check.Origin)
		if check.Name == "" || !validCheckOrigin(check.Origin) {
			return nil, false
		}
		out = append(out, check)
	}
	return out, true
}

// NormalizeRequiredChecks validates and encodes a configured list for storage.
// An empty list is stored as [] so the gate never has to distinguish NULL.
func NormalizeRequiredChecks(checks []RequiredCheck) (datatypes.JSON, error) {
	out := make([]RequiredCheck, 0, len(checks))
	for _, check := range checks {
		check.Name = strings.TrimSpace(check.Name)
		check.Origin = strings.TrimSpace(check.Origin)
		if check.Name == "" {
			return nil, errors.New("required check name is empty")
		}
		if !validCheckOrigin(check.Origin) {
			return nil, fmt.Errorf("required check %q has invalid origin %q", check.Name, check.Origin)
		}
		out = append(out, check)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode required checks: %w", err)
	}
	return datatypes.JSON(raw), nil
}

func validCheckOrigin(origin string) bool {
	if origin == "" || origin == "status" {
		return true
	}
	id, ok := strings.CutPrefix(origin, "app:")
	if !ok {
		return false
	}
	parsed, err := strconv.ParseInt(id, 10, 64)
	return err == nil && parsed > 0
}

// EvaluateConfiguredCI is EvaluateCI with the raw policy column. A policy that
// cannot be decoded yields an unknown verdict instead of a weaker one.
func EvaluateConfiguredCI(obs *ValidationObservation, legacy PRValidations, raw datatypes.JSON) CIVerdict {
	required, ok := RequiredChecks(raw)
	if !ok {
		return CIVerdict{State: CIUnknown, Reason: "required_checks policy is not readable"}
	}
	return EvaluateCI(obs, legacy, required)
}

// EvaluateCI decides the gate from one observation. legacy carries the
// counters of rows written before the named observation existed; it is only
// consulted when obs is nil, and it can never prove that a configured required
// check is present.
func EvaluateCI(obs *ValidationObservation, legacy PRValidations, required []RequiredCheck) CIVerdict {
	if obs == nil {
		if legacy.Total == 0 {
			return CIVerdict{State: CIUnknown, Reason: "CI has not been observed for this head"}
		}
		if len(required) > 0 {
			return CIVerdict{State: CIUnknown, Reason: "required checks need a named observation"}
		}
		switch {
		case legacy.Failed > 0:
			return CIVerdict{State: CIFailed, Reason: "a check failed"}
		case legacy.Passed < legacy.Total:
			return CIVerdict{State: CIPending, Reason: "a check is still running"}
		default:
			return CIVerdict{State: CIGreen, Reason: "all observed checks passed"}
		}
	}
	if obs.Result != ObservationOK {
		return CIVerdict{State: CIUnknown, Reason: "CI lookup failed; the state of the head is unknown"}
	}

	if len(required) > 0 {
		return evaluateRequired(*obs, required)
	}
	if len(obs.Checks) == 0 {
		return CIVerdict{State: CIUnknown, Reason: "GitHub reported no checks for this head"}
	}
	return evaluateAll(obs.Checks)
}

func evaluateRequired(obs ValidationObservation, required []RequiredCheck) CIVerdict {
	var missing []string
	var pending, failed bool
	for _, req := range required {
		matched := false
		for _, check := range obs.Checks {
			if !strings.EqualFold(check.Name, req.Name) {
				continue
			}
			if req.Origin != "" && check.Origin != req.Origin {
				continue
			}
			matched = true
			switch check.State {
			case CheckFailed:
				failed = true
			case CheckPending:
				pending = true
			}
		}
		if !matched {
			missing = append(missing, req.Name)
		}
	}

	switch {
	case failed:
		return CIVerdict{State: CIFailed, Reason: "a required check failed", Missing: missing}
	case pending:
		return CIVerdict{State: CIPending, Reason: "a required check is still running", Missing: missing}
	case len(missing) > 0:
		return CIVerdict{State: CIAbsent, Reason: "a required check was not reported", Missing: missing}
	default:
		return CIVerdict{State: CIGreen, Reason: "every required check passed"}
	}
}

func evaluateAll(checks []CheckObservation) CIVerdict {
	pending := false
	for _, check := range checks {
		switch check.State {
		case CheckFailed:
			return CIVerdict{State: CIFailed, Reason: "a check failed"}
		case CheckPending:
			pending = true
		}
	}
	if pending {
		return CIVerdict{State: CIPending, Reason: "a check is still running"}
	}
	return CIVerdict{State: CIGreen, Reason: "all observed checks passed"}
}

// CIVerdictFor resolves the stored observation of headSHA and applies the
// configured required checks. A head the pull request has moved past is
// unknown.
func (s *Store) CIVerdictFor(ctx context.Context, prID uint, headSHA string, required datatypes.JSON) (CIVerdict, error) {
	var pr PullRequest
	err := quietDB(s.DB.WithContext(ctx)).Select(
		"id", "head_sha", "validations_passed", "validations_failed", "validations_total", "validations_detail",
	).Take(&pr, prID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CIVerdict{}, fmt.Errorf("pull request %d: %w", prID, ErrNotFound)
	}
	if err != nil {
		return CIVerdict{}, fmt.Errorf("find pull request %d: %w", prID, err)
	}
	if pr.HeadSHA == "" || pr.HeadSHA != headSHA {
		return CIVerdict{State: CIUnknown, Reason: "the observation belongs to another head"}, nil
	}
	obs := decodeObservation(pr.ValidationsDetail)
	legacy := PRValidations{
		Passed: pr.ValidationsPassed, Failed: pr.ValidationsFailed, Total: pr.ValidationsTotal,
	}
	return EvaluateConfiguredCI(obs, legacy, required), nil
}

// PRValidationObservation returns the stored named observation of headSHA, or
// nil when it was never recorded or belongs to another head.
func (s *Store) PRValidationObservation(ctx context.Context, prID uint, headSHA string) (*ValidationObservation, error) {
	var pr PullRequest
	err := quietDB(s.DB.WithContext(ctx)).Select(
		"id", "head_sha", "validations_detail",
	).Take(&pr, prID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("pull request %d: %w", prID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("find pull request %d: %w", prID, err)
	}
	if headSHA == "" || pr.HeadSHA != headSHA {
		return nil, nil
	}
	return decodeObservation(pr.ValidationsDetail), nil
}

func decodeObservation(raw datatypes.JSON) *ValidationObservation {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var obs ValidationObservation
	if err := json.Unmarshal(raw, &obs); err != nil {
		return nil
	}
	return &obs
}

// SetPRValidationObservation stores the counters and the named observation of
// headSHA. A write for a head the pull request no longer points at, or one
// older than the stored observation, is discarded. changed reports whether
// anything stored actually moved.
func (s *Store) SetPRValidationObservation(ctx context.Context, prID uint, headSHA string, v PRValidations, obs ValidationObservation, stamp ObservationStamp) (bool, error) {
	if obs.Result == "" {
		obs.Result = ObservationOK
	}
	raw, err := json.Marshal(obs)
	if err != nil {
		return false, fmt.Errorf("encode validations of pull request %d: %w", prID, err)
	}
	return s.storeValidations(ctx, prID, headSHA, v, datatypes.JSON(raw), stamp)
}

// SetPRValidationLookupFailure records that the check lookup for headSHA
// failed. Counters are cleared so the previous verdict cannot be mistaken for
// the current one.
func (s *Store) SetPRValidationLookupFailure(ctx context.Context, prID uint, headSHA string, stamp ObservationStamp) (bool, error) {
	raw, err := json.Marshal(ValidationObservation{Result: ObservationLookupFailed})
	if err != nil {
		return false, fmt.Errorf("encode validation failure of pull request %d: %w", prID, err)
	}
	return s.storeValidations(ctx, prID, headSHA, PRValidations{}, datatypes.JSON(raw), stamp)
}

func normalizeObservationStamp(stamp ObservationStamp) ObservationStamp {
	if stamp.At.IsZero() {
		stamp.At = time.Now().UTC()
	}
	stamp.At = stamp.At.UTC().Truncate(time.Microsecond)
	if stamp.Source == "" {
		stamp.Source = ObservationSourceUnknown
	}
	return stamp
}

func (s *Store) storeValidations(ctx context.Context, prID uint, headSHA string, v PRValidations, detail datatypes.JSON, stamp ObservationStamp) (bool, error) {
	stamp = normalizeObservationStamp(stamp)
	var current PullRequest
	err := quietDB(s.DB.WithContext(ctx)).Select(
		"id", "head_sha", "validations_passed", "validations_failed", "validations_total", "validations_detail",
		"validations_observed_at",
	).Take(&current, prID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, fmt.Errorf("pull request %d: %w", prID, ErrNotFound)
	}
	if err != nil {
		return false, fmt.Errorf("find pull request %d: %w", prID, err)
	}
	if headSHA == "" || current.HeadSHA != headSHA {
		return false, nil
	}
	// A reading that started before the stored one was already superseded on its
	// way back. Letting it land would re-open a verdict a newer delivery fixed.
	if !current.ValidationsObservedAt.IsZero() && stamp.At.Before(current.ValidationsObservedAt.UTC()) {
		return false, nil
	}
	same := current.ValidationsPassed == v.Passed &&
		current.ValidationsFailed == v.Failed &&
		current.ValidationsTotal == v.Total &&
		sameObservation(current.ValidationsDetail, detail)
	if same && !stamp.At.After(current.ValidationsObservedAt.UTC()) {
		return false, nil
	}

	updates := map[string]any{
		"validations_observed_at": stamp.At,
		"validations_source":      stamp.Source,
	}
	if !same {
		updates["validations_passed"] = v.Passed
		updates["validations_failed"] = v.Failed
		updates["validations_total"] = v.Total
		updates["validations_detail"] = detail
		updates["updated_at"] = time.Now().UTC()
	}

	res := s.DB.WithContext(ctx).Model(&PullRequest{}).
		Where("id = ? AND head_sha = ? AND (validations_observed_at IS NULL OR validations_observed_at <= ?)",
			prID, headSHA, stamp.At).
		Updates(updates)
	if res.Error != nil {
		return false, fmt.Errorf("set validations of pull request %d: %w", prID, res.Error)
	}
	if res.RowsAffected == 0 {
		// A newer reading won the race after the row was read.
		return false, nil
	}
	return !same, nil
}

// sameObservation compares the decoded observations: jsonb normalizes key
// order and whitespace, so a byte comparison would always look changed.
func sameObservation(a, b datatypes.JSON) bool {
	var left, right ValidationObservation
	if err := json.Unmarshal(a, &left); err != nil {
		return string(a) == string(b)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		return string(a) == string(b)
	}
	return reflect.DeepEqual(left, right)
}

// OpenPRsForHeadSHA lists the open pull requests of a repository at one head.
// Several pull requests can share a commit, so a check delivery must be applied
// to all of them.
func (s *Store) OpenPRsForHeadSHA(ctx context.Context, repoID int64, headSHA string) ([]PullRequest, error) {
	var rows []PullRequest
	err := s.DB.WithContext(ctx).
		Where("repository_id = ? AND head_sha = ? AND state = ?", repoID, headSHA, stateOpen).
		Order("id").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list pull requests at %q: %w", headSHA, err)
	}
	return rows, nil
}
