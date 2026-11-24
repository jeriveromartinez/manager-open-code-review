package github

import "context"

// ObjectionEnqueuer schedules the two database-backed steps of an objection:
// the worker's bounded evaluation and the publisher's GitHub reply. The app has
// no OCR, so the webhook never evaluates inline.
type ObjectionEnqueuer interface {
	EnqueueObjection(ctx context.Context, objectionID uint) error
	EnqueueObjectionPublish(ctx context.Context, objectionID uint) error
}
