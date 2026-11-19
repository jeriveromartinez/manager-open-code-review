package jobs_test

import (
	"bytes"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/jobs"
	"github.com/jeriveromartinez/qa-control-center/internal/review"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// TestNewReviewClientBuilds verifies that the review process can build a River
// client from its runner, its profile checker and the configured concurrency.
func TestNewReviewClientBuilds(t *testing.T) {
	s := testutil.NewStore(t)
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("create crypto box: %v", err)
	}

	runner := &review.Runner{Store: s, Box: box}
	checker := &review.ProfileChecker{Store: s, Box: box}
	client, err := jobs.NewReviewClient(s.Pool, runner, checker, 2)
	if err != nil {
		t.Fatalf("NewReviewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewReviewClient() returned a nil client")
	}
}
