package api

import (
	"context"
	"log/slog"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
)

// notify tells the open panels that ev changed. The write it announces is
// already stored, so a failed notification is logged instead of failing the
// request.
func (s *server) notify(ctx context.Context, ev events.Event) {
	if s.Events == nil {
		return
	}
	if err := s.Events.Publish(ctx, ev); err != nil {
		slog.Warn("publish event", "type", ev.Type, "id", ev.ID, "error", err)
	}
}
