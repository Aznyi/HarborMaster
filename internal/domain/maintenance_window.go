package domain

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Maintenance windows.
//
// # Why this is harder than it looks
//
// "Update between 02:00 and 04:00 on weekends" is three separate problems:
//
//  1. WHOSE 02:00. A server in UTC and an operator in Europe/London disagree
//     for half the year. The window carries an IANA zone and every comparison
//     is made in it, never in the server's local time.
//  2. WHICH DAY, when the window crosses midnight. A 22:00-02:00 window on
//     "Saturday" runs into Sunday morning, and the weekday that matters is the
//     one the window STARTED on.
//  3. WHAT HAPPENS ON A DST BOUNDARY. On a spring-forward night 02:30 does not
//     exist; on an autumn-back night it happens twice. Go's time package
//     resolves both correctly when the comparison is done on a zoned instant
//     rather than on wall-clock arithmetic, which is why this file converts to
//     the zone first and compares minutes-of-day after.
//
// # Fail closed
//
// A window whose zone cannot be loaded is CLOSED, not open. Getting this
// backwards would mean a mistyped timezone silently authorised updates at any
// hour, which is the opposite of what a maintenance window is for.

// ErrWindowZone reports an unloadable timezone.
var ErrWindowZone = errors.New("the maintenance window names a timezone this host cannot resolve")

// MaintenanceWindow is when automation may act.
type MaintenanceWindow struct {
	// AlwaysOpen disables the window entirely. Explicit rather than implied by
	// empty fields: "no window configured" and "deliberately unrestricted" are
	// different intentions and an operator should have to state the second.
	AlwaysOpen bool `json:"alwaysOpen"`

	// Timezone is an IANA name, e.g. "Europe/London". Empty means UTC.
	Timezone string `json:"timezone,omitempty"`
	// Weekdays the window may START on, as time.Weekday values (0 = Sunday).
	// Empty means every day.
	Weekdays []int `json:"weekdays,omitempty"`
	// Start and End are "HH:MM" in the window's own timezone. End before Start
	// means the window crosses midnight.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

// minutesOfDay parses "HH:MM" into minutes past midnight.
func minutesOfDay(value string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, false
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

// Location resolves the window's timezone.
//
// Note the build imports `time/tzdata` in the binary's main package: the
// runtime image is distroless and carries no system zoneinfo, so without the
// embedded database every named zone on a real deployment would fail to load
// and every window would be closed.
func (w MaintenanceWindow) Location() (*time.Location, error) {
	if strings.TrimSpace(w.Timezone) == "" {
		return time.UTC, nil
	}
	location, err := time.LoadLocation(w.Timezone)
	if err != nil {
		return nil, ErrWindowZone
	}
	return location, nil
}

// CrossesMidnight reports whether the window wraps past 00:00.
func (w MaintenanceWindow) CrossesMidnight() bool {
	start, okStart := minutesOfDay(w.Start)
	end, okEnd := minutesOfDay(w.End)
	return okStart && okEnd && end < start
}

// Open reports whether the window admits work at an instant.
//
// The instant is converted into the window's zone FIRST. Everything after that
// is arithmetic on a wall clock that has already had DST applied to it, which
// is what makes a spring-forward gap and an autumn-back repeat both come out
// right without either being special-cased.
func (w MaintenanceWindow) Open(at time.Time) (bool, error) {
	if w.AlwaysOpen {
		return true, nil
	}

	location, err := w.Location()
	if err != nil {
		// Fail CLOSED. A window nobody can evaluate authorises nothing.
		return false, err
	}

	start, okStart := minutesOfDay(w.Start)
	end, okEnd := minutesOfDay(w.End)
	if !okStart || !okEnd {
		return false, nil
	}
	// A zero-length window is closed rather than instantaneous.
	if start == end {
		return false, nil
	}

	local := at.In(location)
	nowMinutes := local.Hour()*60 + local.Minute()

	if start < end {
		// An ordinary same-day window.
		if nowMinutes < start || nowMinutes >= end {
			return false, nil
		}
		return w.admitsWeekday(local.Weekday()), nil
	}

	// A window that crosses midnight is two spans, and the WEEKDAY that
	// governs is the one the window opened on.
	switch {
	case nowMinutes >= start:
		// The evening half: today is the start day.
		return w.admitsWeekday(local.Weekday()), nil
	case nowMinutes < end:
		// The morning half: the window started YESTERDAY, so yesterday's
		// weekday is the one that had to be permitted.
		return w.admitsWeekday(local.AddDate(0, 0, -1).Weekday()), nil
	default:
		return false, nil
	}
}

// admitsWeekday reports whether a start day is permitted.
func (w MaintenanceWindow) admitsWeekday(day time.Weekday) bool {
	if len(w.Weekdays) == 0 {
		return true
	}
	for _, allowed := range w.Weekdays {
		if allowed == int(day) {
			return true
		}
	}
	return false
}

// NextOpen returns when the window next admits work, searching forward.
//
// Minute resolution over a bounded horizon of eight days, which is one more
// than the longest possible gap in a weekday-restricted weekly window. A
// window that never opens returns ok=false rather than scanning forever.
//
// Used for the "next maintenance window" the dashboard shows, so an operator
// can see when automation will next be able to act rather than inferring it.
func (w MaintenanceWindow) NextOpen(from time.Time) (time.Time, bool) {
	if w.AlwaysOpen {
		return from, true
	}
	location, err := w.Location()
	if err != nil {
		return time.Time{}, false
	}

	// Truncated to the minute so the answer is stable when asked twice within
	// one minute, which the dashboard does on every poll.
	cursor := from.In(location).Truncate(time.Minute)
	const horizonMinutes = 8 * 24 * 60

	for i := 0; i < horizonMinutes; i++ {
		open, openErr := w.Open(cursor)
		if openErr != nil {
			return time.Time{}, false
		}
		if open {
			return cursor, true
		}
		cursor = cursor.Add(time.Minute)
	}
	return time.Time{}, false
}

// Validate reports why a window is unusable, or nil.
func (w MaintenanceWindow) Validate() error {
	if w.AlwaysOpen {
		return nil
	}
	if _, err := w.Location(); err != nil {
		return err
	}
	if _, ok := minutesOfDay(w.Start); !ok {
		return errors.New("the maintenance window's start must be HH:MM")
	}
	if _, ok := minutesOfDay(w.End); !ok {
		return errors.New("the maintenance window's end must be HH:MM")
	}
	start, _ := minutesOfDay(w.Start)
	end, _ := minutesOfDay(w.End)
	if start == end {
		return errors.New("the maintenance window's start and end are the same, so it never opens")
	}
	for _, day := range w.Weekdays {
		if day < 0 || day > 6 {
			return errors.New("a maintenance window weekday must be 0 (Sunday) to 6 (Saturday)")
		}
	}
	return nil
}

// Describe renders the window for an operator, in HarborMaster's own words.
func (w MaintenanceWindow) Describe() string {
	if w.AlwaysOpen {
		return "at any time"
	}
	zone := w.Timezone
	if zone == "" {
		zone = "UTC"
	}
	days := "every day"
	if len(w.Weekdays) > 0 {
		names := make([]string, 0, len(w.Weekdays))
		for _, day := range w.Weekdays {
			if day >= 0 && day <= 6 {
				names = append(names, time.Weekday(day).String())
			}
		}
		days = strings.Join(names, ", ")
	}
	crossing := ""
	if w.CrossesMidnight() {
		crossing = " (crossing midnight)"
	}
	return w.Start + "-" + w.End + crossing + " " + zone + ", " + days
}
