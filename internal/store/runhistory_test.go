package store_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// historyRun seeds one terminal run with an explicit creation time so the
// ordering assertions do not depend on how fast the inserts happen.
func (f *fixture) historyRun(pr store.PullRequest, status, trigger string, createdAt time.Time) store.ReviewRun {
	f.t.Helper()

	run := store.ReviewRun{
		PullRequestID: pr.ID,
		HeadSHA:       pr.HeadSHA,
		Status:        status,
		Trigger:       trigger,
		Attempt:       1,
		CreatedAt:     createdAt,
	}
	if err := f.s.DB.WithContext(f.ctx).Create(&run).Error; err != nil {
		f.t.Fatalf("create %s history run: %v", status, err)
	}
	return run
}

func historyIDs(page store.RunHistoryPage) []uint {
	if len(page.Runs) == 0 {
		return nil
	}
	ids := make([]uint, len(page.Runs))
	for i, r := range page.Runs {
		ids[i] = r.ID
	}
	return ids
}

func historySetup(t *testing.T) (f *fixture, repo store.Repository, team store.Team, admin store.User) {
	t.Helper()

	f = newFixture(t)
	inst := f.nextID()
	f.installation(inst)
	team = f.team("alpha")
	repo = f.repo(f.nextID(), inst, "acme/app", &team.ID, true)
	admin = f.user("root", true)
	return f, repo, team, admin
}

func TestReviewRunHistoryReturnsTerminalRunsNewestFirst(t *testing.T) {
	f, repo, _, admin := historySetup(t)
	base := time.Now().UTC().Truncate(time.Millisecond)

	closedPR := f.openPR(repo.ID, 1, store.StatusPending, false)
	oldest := f.historyRun(closedPR, store.RunSucceeded, "webhook", base.Add(-4*time.Minute))
	if err := f.s.DB.WithContext(f.ctx).Exec(
		"UPDATE pull_requests SET state = 'closed' WHERE id = ?", closedPR.ID).Error; err != nil {
		t.Fatalf("close pull request: %v", err)
	}

	failedPR := f.openPR(repo.ID, 2, store.StatusPending, false)
	failed := f.historyRun(failedPR, store.RunFailed, "manual", base.Add(-3*time.Minute))
	started := base.Add(-3 * time.Minute)
	finished := base.Add(-2 * time.Minute)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).Where("id = ?", failed.ID).
		Updates(map[string]any{
			"error_code":    "boom",
			"error_message": "it broke",
			"started_at":    started,
			"finished_at":   finished,
		}).Error; err != nil {
		t.Fatalf("annotate failed run: %v", err)
	}

	inconclusivePR := f.openPR(repo.ID, 3, store.StatusPending, false)
	inconclusive := f.historyRun(inconclusivePR, store.RunInconclusive, "webhook", base.Add(-2*time.Minute))

	supersededPR := f.openPR(repo.ID, 4, store.StatusPending, false)
	newest := f.historyRun(supersededPR, store.RunSuperseded, "webhook", base)

	// Active runs belong to the live view, never to the history.
	f.run(f.openPR(repo.ID, 5, store.StatusPending, false), store.RunQueued)
	f.run(f.openPR(repo.ID, 6, store.StatusPending, false), store.RunRunning)

	page, err := f.s.ReviewRunHistory(f.ctx, admin, store.RunHistoryFilter{})
	if err != nil {
		t.Fatalf("ReviewRunHistory() error = %v", err)
	}

	want := []uint{newest.ID, inconclusive.ID, failed.ID, oldest.ID}
	if got := historyIDs(page); !reflect.DeepEqual(got, want) {
		t.Fatalf("run ids = %v, want newest first %v", got, want)
	}
	if page.HasMore {
		t.Error("HasMore = true, want false for a single full page")
	}
	if page.NextBefore != int64(oldest.ID) {
		t.Errorf("NextBefore = %d, want the last returned id %d", page.NextBefore, oldest.ID)
	}
	if page.NextCreatedAt == nil || !page.NextCreatedAt.Equal(oldest.CreatedAt) {
		t.Errorf("NextCreatedAt = %v, want the last returned creation time %v", page.NextCreatedAt, oldest.CreatedAt)
	}

	if untouched := page.Runs[3]; untouched.StartedAt != nil || untouched.FinishedAt != nil {
		t.Errorf("run without timestamps decoded as %v/%v, want nil/nil",
			untouched.StartedAt, untouched.FinishedAt)
	}

	row := page.Runs[2]
	if row.PullRequestID != failedPR.ID || row.PRNumber != 2 || row.PRTitle != "PR 2" {
		t.Errorf("failed row = %+v, want pull request 2 (PR 2)", row)
	}
	if row.Repo != "acme/app" || row.Team != "alpha" || row.Profile != "" {
		t.Errorf("failed row = %+v, want repo acme/app and team alpha", row)
	}
	if row.Status != store.RunFailed || row.Trigger != "manual" || row.Attempt != 1 {
		t.Errorf("failed row = %+v, want failed/manual/attempt 1", row)
	}
	if row.ErrorCode != "boom" || row.ErrorMessage != "it broke" {
		t.Errorf("failed row error = %q/%q, want boom/it broke", row.ErrorCode, row.ErrorMessage)
	}
	if row.StartedAt == nil || !row.StartedAt.Equal(started) {
		t.Errorf("failed row StartedAt = %v, want %v", row.StartedAt, started)
	}
	if row.FinishedAt == nil || !row.FinishedAt.Equal(finished) {
		t.Errorf("failed row FinishedAt = %v, want %v", row.FinishedAt, finished)
	}
}

func TestReviewRunHistoryPaginatesWithCursor(t *testing.T) {
	f, repo, _, admin := historySetup(t)
	base := time.Now().UTC().Truncate(time.Millisecond)

	var monotonic []uint // oldest to newest by creation time
	for i := 1; i <= 5; i++ {
		pr := f.openPR(repo.ID, i, store.StatusPending, false)
		run := f.historyRun(pr, store.RunSucceeded, "webhook", base.Add(time.Duration(i)*time.Minute))
		monotonic = append(monotonic, run.ID)
	}
	// Inserted last, so its id is the highest, but its creation time is the
	// oldest. An id-only cursor would skip it.
	outlierPR := f.openPR(repo.ID, 6, store.StatusPending, false)
	outlier := f.historyRun(outlierPR, store.RunSucceeded, "webhook", base.Add(-time.Minute))

	want := []uint{
		monotonic[4], monotonic[3], monotonic[2], monotonic[1], monotonic[0], outlier.ID,
	}

	var got []uint
	seen := make(map[uint]bool)
	var beforeID int64
	var beforeAt *time.Time
	pages := 0
	for {
		page, err := f.s.ReviewRunHistory(f.ctx, admin, store.RunHistoryFilter{
			BeforeID: beforeID, BeforeCreatedAt: beforeAt, Limit: 2,
		})
		if err != nil {
			t.Fatalf("ReviewRunHistory(page %d) error = %v", pages+1, err)
		}
		pages++

		ids := historyIDs(page)
		if len(ids) == 0 {
			t.Fatalf("page %d is empty before the cursor ran out", pages)
		}
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("run %d repeated across pages", id)
			}
			seen[id] = true
			got = append(got, id)
		}
		last := page.Runs[len(page.Runs)-1]
		if page.NextBefore != int64(last.ID) {
			t.Errorf("page %d NextBefore = %d, want last returned id %d", pages, page.NextBefore, last.ID)
		}
		if page.NextCreatedAt == nil || !page.NextCreatedAt.Equal(last.CreatedAt) {
			t.Errorf("page %d NextCreatedAt = %v, want %v", pages, page.NextCreatedAt, last.CreatedAt)
		}
		if !page.HasMore {
			break
		}
		beforeID = page.NextBefore
		beforeAt = page.NextCreatedAt
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged ids = %v, want every run exactly once %v", got, want)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3 for six runs at two per page", pages)
	}

	page, err := f.s.ReviewRunHistory(f.ctx, admin, store.RunHistoryFilter{
		BeforeID: int64(outlier.ID), BeforeCreatedAt: &outlier.CreatedAt, Limit: 2,
	})
	if err != nil {
		t.Fatalf("ReviewRunHistory(past end) error = %v", err)
	}
	if len(page.Runs) != 0 || page.HasMore || page.NextBefore != 0 || page.NextCreatedAt != nil {
		t.Errorf("past-end page = %+v, want empty with no cursor and no more", page)
	}

	// An id without its creation time is not a cursor: it must not skip rows.
	page, err = f.s.ReviewRunHistory(f.ctx, admin, store.RunHistoryFilter{BeforeID: int64(want[0]), Limit: 2})
	if err != nil {
		t.Fatalf("ReviewRunHistory(lone id) error = %v", err)
	}
	if got := historyIDs(page); !reflect.DeepEqual(got, want[:2]) {
		t.Errorf("lone-id page = %v, want the first page %v", got, want[:2])
	}
}

func TestReviewRunHistoryFiltersByStatusRepoAndTrigger(t *testing.T) {
	f, repoA, team, admin := historySetup(t)
	repoB := f.repo(f.nextID(), repoA.InstallationID, "acme/b", &team.ID, true)
	base := time.Now().UTC().Truncate(time.Millisecond)

	aOK := f.historyRun(f.openPR(repoA.ID, 1, store.StatusPending, false),
		store.RunSucceeded, "webhook", base.Add(-4*time.Minute))
	aFailed := f.historyRun(f.openPR(repoA.ID, 2, store.StatusPending, false),
		store.RunFailed, "manual", base.Add(-3*time.Minute))
	bFailed := f.historyRun(f.openPR(repoB.ID, 3, store.StatusPending, false),
		store.RunFailed, "webhook", base.Add(-2*time.Minute))
	bOK := f.historyRun(f.openPR(repoB.ID, 4, store.StatusPending, false),
		store.RunSucceeded, "manual", base.Add(-1*time.Minute))

	cases := []struct {
		name   string
		filter store.RunHistoryFilter
		want   []uint
	}{
		{"status", store.RunHistoryFilter{Statuses: []string{store.RunFailed}}, []uint{bFailed.ID, aFailed.ID}},
		{"repository", store.RunHistoryFilter{RepoID: repoA.ID}, []uint{aFailed.ID, aOK.ID}},
		{"trigger", store.RunHistoryFilter{Trigger: "manual"}, []uint{bOK.ID, aFailed.ID}},
		{"combined", store.RunHistoryFilter{Statuses: []string{store.RunFailed}, RepoID: repoA.ID}, []uint{aFailed.ID}},
		{"non-terminal status", store.RunHistoryFilter{Statuses: []string{store.RunQueued, store.RunRunning}}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := f.s.ReviewRunHistory(f.ctx, admin, tc.filter)
			if err != nil {
				t.Fatalf("ReviewRunHistory(%s) error = %v", tc.name, err)
			}
			if got := historyIDs(page); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("run ids = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReviewRunHistorySearchesTitleAndRepo(t *testing.T) {
	f, repoA, team, admin := historySetup(t)
	repoB := f.repo(f.nextID(), repoA.InstallationID, "acme/searchtarget", &team.ID, true)
	base := time.Now().UTC().Truncate(time.Millisecond)

	plainPR := f.openPR(repoA.ID, 10, store.StatusPending, false)
	plain := f.historyRun(plainPR, store.RunSucceeded, "webhook", base.Add(-3*time.Minute))

	itemsPR := f.openPR(repoA.ID, 12, store.StatusPending, false)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Where("id = ?", itemsPR.ID).
		Update("title", "100 items").Error; err != nil {
		t.Fatalf("rename pull request: %v", err)
	}
	items := f.historyRun(itemsPR, store.RunSucceeded, "webhook", base.Add(-2*time.Minute))

	titlePR := f.openPR(repoB.ID, 11, store.StatusPending, false)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Where("id = ?", titlePR.ID).
		Update("title", "Fix 100% of widgets").Error; err != nil {
		t.Fatalf("rename pull request: %v", err)
	}
	titled := f.historyRun(titlePR, store.RunSucceeded, "webhook", base.Add(-1*time.Minute))

	cases := []struct {
		name  string
		query string
		want  []uint
	}{
		{"repository", "searchtarget", []uint{titled.ID}},
		{"title case-insensitive", "WIDGET", []uint{titled.ID}},
		{"title by number", "pr 10", []uint{plain.ID}},
		{"literal percent", "100%", []uint{titled.ID}},
		{"account matches all", "acme/", []uint{titled.ID, items.ID, plain.ID}},
		{"no match", "nothing-here", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := f.s.ReviewRunHistory(f.ctx, admin, store.RunHistoryFilter{Query: tc.query})
			if err != nil {
				t.Fatalf("ReviewRunHistory(%q) error = %v", tc.query, err)
			}
			if got := historyIDs(page); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("run ids for %q = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestReviewRunHistoryRespectsTeamScope(t *testing.T) {
	f := newFixture(t)
	inst := f.nextID()
	f.installation(inst)
	teamA := f.team("alpha")
	teamB := f.team("beta")
	repoA := f.repo(f.nextID(), inst, "acme/a", &teamA.ID, true)
	repoB := f.repo(f.nextID(), inst, "acme/b", &teamB.ID, true)
	repoOff := f.repo(f.nextID(), inst, "acme/off", &teamA.ID, false)
	devA := f.user("deva", false)
	f.member(teamA, devA, "developer")
	admin := f.user("root", true)
	base := time.Now().UTC().Truncate(time.Millisecond)

	runA := f.historyRun(f.openPR(repoA.ID, 1, store.StatusPending, false),
		store.RunSucceeded, "webhook", base.Add(-3*time.Minute))
	runB := f.historyRun(f.openPR(repoB.ID, 2, store.StatusPending, false),
		store.RunSucceeded, "webhook", base.Add(-2*time.Minute))
	f.historyRun(f.openPR(repoOff.ID, 3, store.StatusPending, false),
		store.RunSucceeded, "webhook", base.Add(-1*time.Minute))

	page, err := f.s.ReviewRunHistory(f.ctx, devA, store.RunHistoryFilter{})
	if err != nil {
		t.Fatalf("ReviewRunHistory(dev) error = %v", err)
	}
	if got := historyIDs(page); !reflect.DeepEqual(got, []uint{runA.ID}) {
		t.Errorf("dev runs = %v, want only the team's repository", got)
	}

	page, err = f.s.ReviewRunHistory(f.ctx, devA, store.RunHistoryFilter{RepoID: repoB.ID})
	if err != nil {
		t.Fatalf("ReviewRunHistory(dev, other repo) error = %v", err)
	}
	if len(page.Runs) != 0 {
		t.Errorf("dev asking for another team's repo got %v, want nothing", historyIDs(page))
	}

	page, err = f.s.ReviewRunHistory(f.ctx, admin, store.RunHistoryFilter{})
	if err != nil {
		t.Fatalf("ReviewRunHistory(admin) error = %v", err)
	}
	if got, want := historyIDs(page), []uint{runB.ID, runA.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("admin runs = %v, want both enabled repositories %v", got, want)
	}
}

func TestReviewRunHistoryExcludesActiveRuns(t *testing.T) {
	f, repo, _, admin := historySetup(t)
	base := time.Now().UTC().Truncate(time.Millisecond)

	done := f.historyRun(f.openPR(repo.ID, 1, store.StatusPending, false),
		store.RunSucceeded, "webhook", base.Add(-2*time.Minute))
	f.run(f.openPR(repo.ID, 2, store.StatusPending, false), store.RunQueued)
	f.run(f.openPR(repo.ID, 3, store.StatusPending, false), store.RunRunning)

	page, err := f.s.ReviewRunHistory(f.ctx, admin, store.RunHistoryFilter{})
	if err != nil {
		t.Fatalf("ReviewRunHistory() error = %v", err)
	}
	if got := historyIDs(page); !reflect.DeepEqual(got, []uint{done.ID}) {
		t.Errorf("run ids = %v, want only the finished run", got)
	}

	page, err = f.s.ReviewRunHistory(f.ctx, admin,
		store.RunHistoryFilter{Statuses: []string{store.RunQueued, store.RunRunning}})
	if err != nil {
		t.Fatalf("ReviewRunHistory(active statuses) error = %v", err)
	}
	if len(page.Runs) != 0 {
		t.Errorf("filtering by active statuses returned %v, want nothing", historyIDs(page))
	}
}

func TestReviewRunHistoryClampsLimit(t *testing.T) {
	f, repo, _, admin := historySetup(t)
	base := time.Now().UTC().Truncate(time.Millisecond)

	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	runs := make([]store.ReviewRun, 0, 205)
	for i := 0; i < 205; i++ {
		runs = append(runs, store.ReviewRun{
			PullRequestID: pr.ID,
			HeadSHA:       pr.HeadSHA,
			Status:        store.RunSucceeded,
			Trigger:       "webhook",
			Attempt:       1,
			CreatedAt:     base.Add(time.Duration(i) * time.Second),
		})
	}
	if err := f.s.DB.WithContext(f.ctx).CreateInBatches(&runs, 50).Error; err != nil {
		t.Fatalf("seed runs: %v", err)
	}

	cases := []struct {
		name     string
		limit    int
		wantRows int
		wantMore bool
	}{
		{"default", 0, 50, true},
		{"negative falls back to default", -5, 50, true},
		{"maximum", 1000, 200, true},
		{"minimum", 1, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := f.s.ReviewRunHistory(f.ctx, admin, store.RunHistoryFilter{Limit: tc.limit})
			if err != nil {
				t.Fatalf("ReviewRunHistory(limit %d) error = %v", tc.limit, err)
			}
			if len(page.Runs) != tc.wantRows {
				t.Errorf("rows = %d, want %d", len(page.Runs), tc.wantRows)
			}
			if page.HasMore != tc.wantMore {
				t.Errorf("HasMore = %v, want %v", page.HasMore, tc.wantMore)
			}
		})
	}
}
