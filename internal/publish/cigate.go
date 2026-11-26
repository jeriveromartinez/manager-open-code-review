package publish

import (
	"context"
	"errors"
	"log/slog"
	"time"

	appgithub "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// ciVerdict is the single gate decision used by publication and republication.
// With the gate disabled it is green by construction; with it enabled the
// checks are re-read from GitHub so a head that turned red after the review was
// scheduled can never publish a success. A failed lookup yields CIUnknown, not
// a green verdict.
func (p *publishContext) ciVerdict(ctx context.Context) (store.CIVerdict, error) {
	if !p.policy.RequireGreenCI {
		return store.CIVerdict{State: store.CIGreen, Reason: "CI gate disabled"}, nil
	}
	// The stamp is taken before the lookup so this reading cannot claim to be
	// newer than a delivery that arrived while it was in flight.
	stamp := store.ObservationStamp{At: time.Now().UTC(), Source: store.ObservationSourcePublish}
	validations, observation, lookupErr := appgithub.ReadObservation(
		ctx, p.client, p.owner, p.name, p.run.HeadSHA, p.app.Creds.AppID,
	)
	if lookupErr != nil {
		// Do not leave the previous verdict behind: the trigger must not use a
		// green observation this publication could not confirm.
		write, markErr := p.w.Store.SetPRValidationLookupFailure(ctx, p.pr.ID, p.run.HeadSHA, stamp)
		if markErr != nil {
			slog.Warn("store validation lookup failure", "pull_request_id", p.pr.ID, "error", markErr)
			return store.CIVerdict{State: store.CIUnknown, Reason: "CI lookup failed"}, errors.Join(lookupErr, markErr)
		}
		if !write.Accepted {
			// A newer reading superseded this failed lookup while it was in
			// flight. Publishing this one would contradict the stored verdict.
			return p.storedCIVerdict(ctx, lookupErr)
		}
		return store.CIVerdict{State: store.CIUnknown, Reason: "CI lookup failed"}, lookupErr
	}
	// Keep the stored observation current so the panel and the trigger use the
	// same verdict this publication saw. The write is head- and stamp-conditional.
	write, storeErr := p.w.Store.SetPRValidationObservation(
		ctx, p.pr.ID, p.run.HeadSHA, validations, observation, stamp,
	)
	if storeErr != nil {
		slog.Warn("store validations at publish", "pull_request_id", p.pr.ID, "error", storeErr)
		return store.EvaluateConfiguredCI(&observation, validations, p.policy.RequiredChecks), nil
	}
	if !write.Accepted {
		// The stored observation is newer than this read; its verdict is the one
		// GitHub already reflects, so do not patch the check back to this one.
		return p.storedCIVerdict(ctx, nil)
	}
	return store.EvaluateConfiguredCI(&observation, validations, p.policy.RequiredChecks), nil
}

// storedCIVerdict resolves the verdict of the observation another writer stored
// while this publication's read was in flight.
func (p *publishContext) storedCIVerdict(ctx context.Context, cause error) (store.CIVerdict, error) {
	verdict, err := p.w.Store.CIVerdictFor(ctx, p.pr.ID, p.run.HeadSHA, p.policy.RequiredChecks)
	if err != nil {
		return store.CIVerdict{State: store.CIUnknown, Reason: "the newer CI observation could not be read"},
			errors.Join(cause, err)
	}
	return verdict, nil
}
