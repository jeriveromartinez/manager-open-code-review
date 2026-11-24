package review

import (
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestIncrementalSourceKeepsComparisonsApart(t *testing.T) {
	tests := []struct {
		name         string
		run          store.ReviewRun
		prev         store.ReviewRun
		reviewedBase string
		headSHA      string
		wantFrom     string
		want         bool
	}{
		{
			name:         "same comparison advances the head",
			run:          store.ReviewRun{BaseRef: "main"},
			prev:         store.ReviewRun{HeadSHA: "old", BaseSHA: "mb"},
			reviewedBase: "mb",
			headSHA:      "new",
			wantFrom:     "old",
			want:         true,
		},
		{
			name:         "retarget with the same merge base reviews the full diff",
			run:          store.ReviewRun{BaseRef: "release"},
			prev:         store.ReviewRun{HeadSHA: "old", BaseRef: "main", BaseSHA: "mb"},
			reviewedBase: "mb",
			headSHA:      "new",
			want:         false,
		},
		{
			name:         "rewritten base with the same head reviews the full diff",
			run:          store.ReviewRun{BaseRef: "main"},
			prev:         store.ReviewRun{HeadSHA: "old", BaseRef: "main", BaseSHA: "mb-old"},
			reviewedBase: "mb-new",
			headSHA:      "new",
			want:         false,
		},
		{
			name:         "same head is never the incremental source",
			run:          store.ReviewRun{BaseRef: "main"},
			prev:         store.ReviewRun{HeadSHA: "same", BaseRef: "main", BaseSHA: "mb"},
			reviewedBase: "mb",
			headSHA:      "same",
			want:         false,
		},
		{
			name:         "legacy comparison without a merge base is not incremental",
			run:          store.ReviewRun{BaseRef: "main"},
			prev:         store.ReviewRun{HeadSHA: "old", BaseRef: "main"},
			reviewedBase: "mb",
			headSHA:      "new",
			want:         false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			from, ok := incrementalSource(tc.run, tc.prev, tc.reviewedBase, tc.headSHA)
			if ok != tc.want || from != tc.wantFrom {
				t.Errorf("incrementalSource() = %q, %v; want %q, %v", from, ok, tc.wantFrom, tc.want)
			}
		})
	}
}
