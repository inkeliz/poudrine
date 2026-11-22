package config

import (
	"fmt"
	"strings"
	"time"
)

// A bit per minute in each local clock hour. The zero value permits all hours.
type WorkingHours [24]uint64

func ParseWorkingHours(value string) (WorkingHours, error) {
	var hours WorkingHours
	if strings.TrimSpace(value) == "" {
		return hours, nil
	}
	for window := range strings.SplitSeq(value, ",") {
		startText, endText, _ := strings.Cut(window, "-")
		start, okStart := clockMinute(strings.TrimSpace(startText))
		end, okEnd := clockMinute(strings.TrimSpace(endText))
		if !okStart || !okEnd || start == 24*60 {
			return WorkingHours{}, fmt.Errorf("working_hours: %q must be a 24-hour range such as 23:00-07:30 (24:00 is valid only as an end time)", strings.TrimSpace(window))
		}
		if start == end {
			return WorkingHours{}, fmt.Errorf("working_hours: %q has identical start and end times; use 00:00-24:00 for a full day", strings.TrimSpace(window))
		}
		if end < start {
			end += 24 * 60
		}
		for minute := start; minute < end; minute++ {
			hours[(minute/60)%24] |= 1 << (minute % 60)
		}
	}
	for _, h := range hours {
		if h != (1<<60)-1 {
			return hours, nil
		}
	}
	return WorkingHours{}, nil
}

func clockMinute(value string) (int, bool) {
	if value == "24:00" {
		return 24 * 60, true
	}
	if len(value) != len("00:00") {
		return 0, false
	}
	t, err := time.Parse("15:04", value)
	return t.Hour()*60 + t.Minute(), err == nil
}

// state returns whether a build can run now and the next opening or closing.
// Walk actual elapsed minutes in now's zone so skipped and repeated local
// clock times on daylight-saving transitions follow the configured windows.
func (w WorkingHours) State(now time.Time) (bool, time.Time) {
	if w == (WorkingHours{}) {
		return true, time.Time{}
	}
	allowed := func(t time.Time) bool {
		hour, minute, _ := t.Clock()
		return w[hour]&(1<<minute) != 0
	}
	current := allowed(now)
	next := now.Add(time.Minute - time.Duration(now.Second())*time.Second - time.Duration(now.Nanosecond()))
	for allowed(next) == current {
		next = next.Add(time.Minute)
	}
	return current, next
}
