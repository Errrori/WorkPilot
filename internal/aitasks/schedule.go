package aitasks

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// DefaultTimezone is used when a task does not specify one.
const DefaultTimezone = "Asia/Shanghai"

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// LoadLocation resolves a task timezone, falling back to the default.
func LoadLocation(timezone string) (*time.Location, error) {
	if strings.TrimSpace(timezone) == "" {
		timezone = DefaultTimezone
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q", timezone)
	}
	return loc, nil
}

// ParseSchedule validates a 5-field cron spec evaluated in the task timezone.
func ParseSchedule(spec, timezone string) (cron.Schedule, *time.Location, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil, fmt.Errorf("schedule is required")
	}
	loc, err := LoadLocation(timezone)
	if err != nil {
		return nil, nil, err
	}
	sched, err := cronParser.Parse(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid schedule %q", spec)
	}
	return sched, loc, nil
}

// NextRun computes the first occurrence strictly after the given time.
func NextRun(spec, timezone string, after time.Time) (time.Time, error) {
	sched, loc, err := ParseSchedule(spec, timezone)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after.In(loc)), nil
}
