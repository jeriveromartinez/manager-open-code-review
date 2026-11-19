package store_test

import (
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestGroupOf(t *testing.T) {
	cases := []struct {
		status string
		group  string
	}{
		{store.StatusDraft, ""},
		{store.StatusAwaitingApproval, "blocked"},
		{store.StatusPending, "in_review"},
		{store.StatusReviewing, "in_review"},
		{store.StatusChangesRequested, "blocked"},
		{store.StatusWaitingCI, "in_review"},
		{store.StatusReadyForHuman, "ready"},
		{store.StatusInconclusive, "blocked"},
		{store.StatusPaused, "blocked"},
		{store.StatusMerged, ""},
		{store.StatusClosed, ""},
	}

	for _, tc := range cases {
		if got := store.GroupOf(tc.status); got != tc.group {
			t.Errorf("GroupOf(%q) = %q, want %q", tc.status, got, tc.group)
		}
	}
}

func TestOnSync(t *testing.T) {
	cases := []struct {
		name string
		prev *store.PullRequest
		in   store.PullRequest
		want string
	}{
		{
			name: "nuevo abierto -> pending",
			prev: nil,
			in:   store.PullRequest{State: "open", HeadSHA: "sha1"},
			want: store.StatusPending,
		},
		{
			name: "nuevo draft -> draft",
			prev: nil,
			in:   store.PullRequest{State: "open", HeadSHA: "sha1", Draft: true},
			want: store.StatusDraft,
		},
		{
			name: "draft -> ready = pending",
			prev: &store.PullRequest{QAStatus: store.StatusDraft, HeadSHA: "sha1"},
			in:   store.PullRequest{State: "open", HeadSHA: "sha1"},
			want: store.StatusPending,
		},
		{
			name: "mismo SHA en changes_requested conserva estado",
			prev: &store.PullRequest{QAStatus: store.StatusChangesRequested, HeadSHA: "sha1"},
			in:   store.PullRequest{State: "open", HeadSHA: "sha1"},
			want: store.StatusChangesRequested,
		},
		{
			name: "SHA nuevo en ready_for_human -> pending",
			prev: &store.PullRequest{QAStatus: store.StatusReadyForHuman, HeadSHA: "sha1"},
			in:   store.PullRequest{State: "open", HeadSHA: "sha2"},
			want: store.StatusPending,
		},
		{
			name: "cerrado -> closed",
			prev: &store.PullRequest{QAStatus: store.StatusReadyForHuman, HeadSHA: "sha1"},
			in:   store.PullRequest{State: store.StatusClosed, HeadSHA: "sha1"},
			want: store.StatusClosed,
		},
		{
			name: "merged -> merged",
			prev: &store.PullRequest{QAStatus: store.StatusPending, HeadSHA: "sha1"},
			in:   store.PullRequest{State: store.StatusMerged, HeadSHA: "sha1"},
			want: store.StatusMerged,
		},
		{
			name: "reabierto desde closed -> pending",
			prev: &store.PullRequest{QAStatus: store.StatusClosed, HeadSHA: "sha1"},
			in:   store.PullRequest{State: "open", HeadSHA: "sha1"},
			want: store.StatusPending,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := store.OnSync(tc.prev, tc.in); got != tc.want {
				t.Errorf("OnSync() = %q, want %q", got, tc.want)
			}
		})
	}
}
