package github_test

import (
	"testing"

	gh "github.com/google/go-github/v93/github"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func checkRun(appID int64, status, conclusion string) *gh.CheckRun {
	run := &gh.CheckRun{Status: gh.String(status), Conclusion: gh.String(conclusion)}
	if appID != 0 {
		run.App = &gh.App{ID: gh.Int64(appID)}
	}
	return run
}

func TestValidationsOf(t *testing.T) {
	const ownAppID int64 = 12345

	tests := []struct {
		name     string
		runs     []*gh.CheckRun
		statuses []*gh.RepoStatus
		want     store.PRValidations
	}{
		{
			name: "all green",
			runs: []*gh.CheckRun{
				checkRun(0, "completed", "success"),
				checkRun(0, "completed", "neutral"),
				checkRun(0, "completed", "skipped"),
			},
			want: store.PRValidations{Passed: 3, Total: 3},
		},
		{
			name: "one failure",
			runs: []*gh.CheckRun{
				checkRun(0, "completed", "success"),
				checkRun(0, "completed", "failure"),
			},
			want: store.PRValidations{Passed: 1, Failed: 1, Total: 2},
		},
		{
			name: "still running",
			runs: []*gh.CheckRun{
				checkRun(0, "completed", "success"),
				checkRun(0, "in_progress", ""),
				checkRun(0, "queued", ""),
			},
			want: store.PRValidations{Passed: 1, Total: 3},
		},
		{
			name: "timed out and cancelled count as failures",
			runs: []*gh.CheckRun{
				checkRun(0, "completed", "timed_out"),
				checkRun(0, "completed", "cancelled"),
			},
			want: store.PRValidations{Failed: 2, Total: 2},
		},
		{
			name: "own check run is not an input",
			runs: []*gh.CheckRun{
				checkRun(0, "completed", "success"),
				checkRun(ownAppID, "completed", "failure"),
				checkRun(ownAppID, "in_progress", ""),
			},
			want: store.PRValidations{Passed: 1, Total: 1},
		},
		{
			name: "no checks at all",
			want: store.PRValidations{},
		},
		{
			name: "commit statuses",
			statuses: []*gh.RepoStatus{
				{State: gh.String("success")},
				{State: gh.String("pending")},
				{State: gh.String("error")},
			},
			want: store.PRValidations{Passed: 1, Failed: 1, Total: 3},
		},
		{
			name: "check runs and statuses combine",
			runs: []*gh.CheckRun{
				checkRun(0, "completed", "success"),
			},
			statuses: []*gh.RepoStatus{
				{State: gh.String("success")},
			},
			want: store.PRValidations{Passed: 2, Total: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ghapp.ValidationsOf(tt.runs, tt.statuses, ownAppID); got != tt.want {
				t.Errorf("ValidationsOf() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
