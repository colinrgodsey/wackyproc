package proc

import (
	"testing"
)

// TestWaitPollIntervalRamp pins the wait-loop polling ramp: the first WaitPollRampCount
// polls run at the start interval (short tasks still detected within a couple of
// intervals of finishing), then the interval doubles per poll until it settles at the
// settle interval.
func TestWaitPollIntervalRamp(t *testing.T) {
	for pollsSoFar, want := range map[int]int{
		0: 100, 5: 100, 9: 100, // first 10 polls stay sharp
		10: 200, 11: 400, 12: 800,
		13: 1000, 14: 1000, 100000: 1000, // settled
	} {
		if got := waitPollIntervalMs(pollsSoFar); got != want {
			t.Errorf("waitPollIntervalMs(%d) = %d, want %d", pollsSoFar, got, want)
		}
	}
	prev := 0
	for i := 0; i < 1000; i++ {
		got := waitPollIntervalMs(i)
		if got < WaitPollStartIntervalMs || got > WaitPollSettleIntervalMs {
			t.Fatalf("waitPollIntervalMs(%d) = %d outside [%d, %d]", i, got, WaitPollStartIntervalMs, WaitPollSettleIntervalMs)
		}
		if got < prev {
			t.Fatalf("waitPollIntervalMs not monotonic at %d: %d < %d", i, got, prev)
		}
		prev = got
	}
}
