package proc

import "testing"

func TestClampWaitSeconds(t *testing.T) {
	cases := []struct {
		requested int
		want      int
	}{
		{requested: 0, want: 1},
		{requested: -5, want: 1},
		{requested: 1, want: 1},
		{requested: 30, want: 30},
		{requested: MaxWaitSeconds, want: MaxWaitSeconds},
		{requested: MaxWaitSeconds + 1, want: MaxWaitSeconds},
		{requested: 999999, want: MaxWaitSeconds},
	}
	for _, c := range cases {
		got := clampWaitSeconds(c.requested)
		if got != c.want {
			t.Errorf("clampWaitSeconds(%d) = %d, want %d", c.requested, got, c.want)
		}
	}
}
