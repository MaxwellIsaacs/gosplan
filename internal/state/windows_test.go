package state

import (
	"context"
	"testing"
	"time"
)

func windowsObs(win, src string, at time.Time, u float64) WindowObservation {
	return WindowObservation{Window: win, Source: src, Utilization: &u, Status: "allowed", ObservedAt: at, Raw: "{}"}
}

func TestWindowsLatestPerWindow(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if err := s.RecordWindows(ctx, []WindowObservation{
		windowsObs("seven_day", "rate_limit_event", t0, 0.1),
		windowsObs("five_hour", "rate_limit_event", t0, 0.2),
		windowsObs("five_hour", "rate_limit_event", t0.Add(time.Hour), 0.5),
		windowsObs("seven_day", "rate_limit_event", t0.Add(-time.Hour), 0.05),
		{Window: "_probe", Status: "error", Source: "usage_text", ObservedAt: t0.Add(2 * time.Hour), Raw: "x"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Window != "five_hour" || got[1].Window != "seven_day" {
		t.Fatalf("got %+v", got)
	}
	if *got[0].Utilization != 0.5 || *got[1].Utilization != 0.1 {
		t.Errorf("not the newest rows: %v %v", *got[0].Utilization, *got[1].Utilization)
	}
	if !got[0].ObservedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("observed_at = %v", got[0].ObservedAt)
	}
}

func TestWindowsLastObservedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if at, err := s.LastObservedAt(ctx); err != nil || !at.IsZero() {
		t.Fatalf("empty: %v %v", at, err)
	}
	t0 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s.RecordWindows(ctx, []WindowObservation{
		windowsObs("session", "usage_text", t0, 0.1),
		{Window: "_probe", Status: "error", Source: "usage_text", ObservedAt: t0.Add(time.Hour), Raw: "x"},
	})
	at, err := s.LastObservedAt(ctx)
	if err != nil || !at.Equal(t0) {
		t.Fatalf("got %v %v, want %v (probe rows excluded)", at, err, t0)
	}
}

func TestWindowsLastProbeAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if at, err := s.LastProbeAt(ctx); err != nil || !at.IsZero() {
		t.Fatalf("empty: %v %v", at, err)
	}
	t0 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s.RecordWindows(ctx, []WindowObservation{
		windowsObs("five_hour", "rate_limit_event", t0.Add(5*time.Hour), 0.1), // not a probe
		windowsObs("session", "usage_text", t0, 0.1),
		{Window: "_probe", Status: "error", Source: "usage_text", ObservedAt: t0.Add(time.Hour), Raw: "x"},
	})
	at, err := s.LastProbeAt(ctx)
	if err != nil || !at.Equal(t0.Add(time.Hour)) {
		t.Fatalf("got %v %v", at, err)
	}
}
