package monitor

import (
	"testing"
	"time"
)

func TestDeliveryBackoffGrowsExponentiallyAndCaps(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 0},
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{6, 16 * time.Minute},
		{7, 30 * time.Minute},
		{10, 30 * time.Minute},
		{50, 30 * time.Minute},
	}
	for _, tt := range tests {
		if got := deliveryBackoff(tt.attempts); got != tt.want {
			t.Errorf("deliveryBackoff(%d) = %s, want %s", tt.attempts, got, tt.want)
		}
	}
}
