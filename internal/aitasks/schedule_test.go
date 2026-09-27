package aitasks

import (
	"testing"
	"time"
)

func TestNextRunWeekdayEvening(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	after := time.Date(2026, 9, 25, 18, 5, 0, 0, time.UTC)

	next, err := NextRun("0 18 * * 5", "Asia/Shanghai", after)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := time.Date(2026, 10, 2, 18, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}

func TestNextRunBeforeFriday(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	after := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	next, err := NextRun("0 18 * * 5", "Asia/Shanghai", after)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := time.Date(2026, 10, 2, 18, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}

func TestNextRunRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name     string
		spec     string
		timezone string
	}{
		{"empty schedule", "", "Asia/Shanghai"},
		{"bad schedule", "every friday", "Asia/Shanghai"},
		{"bad timezone", "0 18 * * 5", "Mars/Olympus"},
	}
	for _, tc := range cases {
		if _, err := NextRun(tc.spec, tc.timezone, time.Now()); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}
}

func TestNextRunDefaultsTimezone(t *testing.T) {
	next, err := NextRun("0 9 * * *", "", time.Now())
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	if next.Location().String() != DefaultTimezone {
		t.Fatalf("location = %s, want %s", next.Location(), DefaultTimezone)
	}
}
