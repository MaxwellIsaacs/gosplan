package core

import (
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, 10 * time.Second},
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{5, 160 * time.Second},
		{6, 5 * time.Minute},
		{100, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := RetryDelay(c.n); got != c.want {
			t.Errorf("RetryDelay(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
