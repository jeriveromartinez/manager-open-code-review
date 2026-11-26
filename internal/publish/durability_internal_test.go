package publish

import (
	"context"
	"testing"
)

// The durable cursor never moves backwards: an older observation must not make
// a retry resend annotations GitHub already accepted (#138).
func TestAdvanceCheckCursorNeverGoesBackwards(t *testing.T) {
	p := &publishContext{progress: publishProgress{CheckAnnotations: 60}}

	if err := p.advanceCheckCursor(context.Background(), 40); err != nil {
		t.Fatalf("advanceCheckCursor(40) error = %v", err)
	}
	if p.progress.CheckAnnotations != 60 {
		t.Errorf("cursor after a lower count = %d, want 60", p.progress.CheckAnnotations)
	}

	if err := p.advanceCheckCursor(context.Background(), 60); err != nil {
		t.Fatalf("advanceCheckCursor(60) error = %v", err)
	}
	if p.progress.CheckAnnotations != 60 {
		t.Errorf("cursor after the same count = %d, want 60", p.progress.CheckAnnotations)
	}
}
