package publish

import (
	"context"
	"log/slog"

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
	validations, observation, err := appgithub.ReadObservation(
		ctx, p.client, p.owner, p.name, p.run.HeadSHA, p.app.Creds.AppID,
	)
	if err != nil {
		// Do not leave the previous verdict behind: the trigger must not use a
		// green observation this publication could not confirm.
		if _, markErr := p.w.Store.SetPRValidationLookupFailure(ctx, p.pr.ID, p.run.HeadSHA); markErr != nil {
			slog.Warn("store validation lookup failure", "pull_request_id", p.pr.ID, "error", markErr)
		}
		return store.CIVerdict{State: store.CIUnknown, Reason: "CI lookup failed"}, err
	}
	// Keep the stored observation current so the panel and the trigger use the
	// same verdict this publication saw. The write is head-conditional.
	if _, storeErr := p.w.Store.SetPRValidationObservation(
		ctx, p.pr.ID, p.run.HeadSHA, validations, observation,
	); storeErr != nil {
		slog.Warn("store validations at publish", "pull_request_id", p.pr.ID, "error", storeErr)
	}
	return store.EvaluateConfiguredCI(&observation, validations, p.policy.RequiredChecks), nil
}
