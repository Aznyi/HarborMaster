package domain

import (
	"testing"
	"time"

	// The tests below name IANA zones, and the CI image is not guaranteed to
	// carry a system zoneinfo database any more than the distroless runtime is.
	_ "time/tzdata"
)

// mustLoad fails the test rather than the assertion when a zone is missing, so
// a missing tzdata reads as an environment problem and not as a window bug.
func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return location
}

func TestWindowAlwaysOpen(t *testing.T) {
	window := MaintenanceWindow{AlwaysOpen: true}
	open, err := window.Open(time.Date(2026, 3, 1, 3, 17, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !open {
		t.Fatal("an always-open window must admit every instant")
	}
}

func TestWindowSameDay(t *testing.T) {
	window := MaintenanceWindow{Start: "02:00", End: "04:00"}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before", time.Date(2026, 3, 1, 1, 59, 0, 0, time.UTC), false},
		{"at start", time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC), true},
		{"inside", time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC), true},
		// The end is exclusive: two adjacent windows must not both admit the
		// same minute, or a container could be picked up by each.
		{"at end", time.Date(2026, 3, 1, 4, 0, 0, 0, time.UTC), false},
		{"after", time.Date(2026, 3, 1, 4, 1, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			open, err := window.Open(tc.at)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if open != tc.want {
				t.Fatalf("open at %s = %v, want %v", tc.at, open, tc.want)
			}
		})
	}
}

func TestWindowRespectsItsOwnTimezoneNotTheServers(t *testing.T) {
	// 02:30 in New York is 07:30 UTC. A window that compared in the server's
	// zone would get both of these backwards.
	window := MaintenanceWindow{Timezone: "America/New_York", Start: "02:00", End: "04:00"}

	inside := time.Date(2026, 3, 10, 7, 30, 0, 0, time.UTC)
	outside := time.Date(2026, 3, 10, 2, 30, 0, 0, time.UTC)

	if open, err := window.Open(inside); err != nil || !open {
		t.Fatalf("07:30 UTC is 02:30 New York and must be inside: open=%v err=%v", open, err)
	}
	if open, err := window.Open(outside); err != nil || open {
		t.Fatalf("02:30 UTC is 21:30 New York the day before and must be outside: open=%v err=%v", open, err)
	}
}

func TestWindowSpringForwardGapDoesNotOpen(t *testing.T) {
	// 2026-03-08, America/New_York: 02:00 jumps straight to 03:00. A window of
	// 02:00-02:59 has no instants at all that night, and must never report one.
	window := MaintenanceWindow{Timezone: "America/New_York", Start: "02:00", End: "02:59"}
	zone := mustLoad(t, "America/New_York")

	// Walk the whole night in UTC and confirm nothing lands in the gap.
	start := time.Date(2026, 3, 8, 4, 0, 0, 0, time.UTC)   // 23:00 the 7th, local
	finish := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC) // 08:00 the 8th, local
	for at := start; at.Before(finish); at = at.Add(time.Minute) {
		open, err := window.Open(at)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if open {
			t.Fatalf("the 02:00-02:59 window opened at %s, but that hour does not exist locally", at.In(zone))
		}
	}
}

func TestWindowAutumnBackRepeatedHourOpensBothTimes(t *testing.T) {
	// 2026-11-01, America/New_York: 01:00-02:00 happens twice. Both are real
	// local instants inside a 01:00-01:59 window, and both must open. Anything
	// that special-cased the repeat would refuse one of the two.
	window := MaintenanceWindow{Timezone: "America/New_York", Start: "01:00", End: "01:59"}

	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)  // 01:30 EDT
	second := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC) // 01:30 EST

	for _, at := range []time.Time{first, second} {
		open, err := window.Open(at)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if !open {
			t.Fatalf("%s is 01:30 local and must be inside the window", at)
		}
	}
}

func TestWindowCrossesMidnight(t *testing.T) {
	window := MaintenanceWindow{Start: "22:00", End: "02:00"}
	if !window.CrossesMidnight() {
		t.Fatal("22:00-02:00 crosses midnight")
	}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before the evening half", time.Date(2026, 3, 1, 21, 59, 0, 0, time.UTC), false},
		{"evening half", time.Date(2026, 3, 1, 23, 30, 0, 0, time.UTC), true},
		{"midnight", time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), true},
		{"morning half", time.Date(2026, 3, 2, 1, 59, 0, 0, time.UTC), true},
		{"at end", time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC), false},
		{"midday", time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			open, err := window.Open(tc.at)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if open != tc.want {
				t.Fatalf("open at %s = %v, want %v", tc.at, open, tc.want)
			}
		})
	}
}

func TestWindowCrossingMidnightUsesTheStartDaysWeekday(t *testing.T) {
	// Saturday only. 2026-03-07 is a Saturday, 2026-03-08 a Sunday.
	window := MaintenanceWindow{
		Start:    "22:00",
		End:      "02:00",
		Weekdays: []int{int(time.Saturday)},
	}

	saturdayEvening := time.Date(2026, 3, 7, 23, 0, 0, 0, time.UTC)
	sundayMorning := time.Date(2026, 3, 8, 1, 0, 0, 0, time.UTC)
	sundayEvening := time.Date(2026, 3, 8, 23, 0, 0, 0, time.UTC)
	saturdayMorning := time.Date(2026, 3, 7, 1, 0, 0, 0, time.UTC)

	if open, _ := window.Open(saturdayEvening); !open {
		t.Fatal("Saturday 23:00 is the evening half of the Saturday window")
	}
	if open, _ := window.Open(sundayMorning); !open {
		t.Fatal("Sunday 01:00 is the morning half of the window that opened on Saturday")
	}
	if open, _ := window.Open(sundayEvening); open {
		t.Fatal("Sunday 23:00 would start a Sunday window, which is not permitted")
	}
	if open, _ := window.Open(saturdayMorning); open {
		t.Fatal("Saturday 01:00 belongs to a window that would have opened on Friday")
	}
}

func TestWindowFailsClosedOnAnUnresolvableZone(t *testing.T) {
	window := MaintenanceWindow{Timezone: "Mars/Olympus_Mons", Start: "02:00", End: "04:00"}

	open, err := window.Open(time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("an unresolvable zone must report an error")
	}
	if open {
		t.Fatal("an unresolvable zone must fail CLOSED; a mistyped timezone must never authorise updates at any hour")
	}
	if _, ok := window.NextOpen(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatal("NextOpen must not claim a window nobody can evaluate will open")
	}
}

func TestWindowZeroLengthNeverOpens(t *testing.T) {
	window := MaintenanceWindow{Start: "02:00", End: "02:00"}
	if open, _ := window.Open(time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)); open {
		t.Fatal("a zero-length window is closed, not instantaneous")
	}
	if err := window.Validate(); err == nil {
		t.Fatal("validation must reject a window that never opens")
	}
}

func TestWindowMalformedTimesAreClosed(t *testing.T) {
	for _, window := range []MaintenanceWindow{
		{Start: "", End: "04:00"},
		{Start: "2:00 AM", End: "04:00"},
		{Start: "25:00", End: "26:00"},
		{Start: "02:60", End: "04:00"},
		{Start: "02-00", End: "04:00"},
	} {
		if open, _ := window.Open(time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)); open {
			t.Fatalf("a window with unparseable times must be closed: %+v", window)
		}
		if err := window.Validate(); err == nil {
			t.Fatalf("validation must reject %+v", window)
		}
	}
}

func TestWindowNextOpen(t *testing.T) {
	window := MaintenanceWindow{Start: "02:00", End: "04:00"}

	from := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	next, ok := window.NextOpen(from)
	if !ok {
		t.Fatal("a daily window always opens again")
	}
	want := time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next open = %s, want %s", next, want)
	}

	// Already inside: the answer is now.
	inside := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	next, ok = window.NextOpen(inside)
	if !ok || !next.Equal(inside) {
		t.Fatalf("inside the window, next open = %s (%v), want %s", next, ok, inside)
	}
}

func TestWindowNextOpenAcrossAWeeklyGap(t *testing.T) {
	// Sundays only, and we ask on a Monday: the answer is six days away, which
	// is why the horizon is eight days rather than two.
	window := MaintenanceWindow{Start: "02:00", End: "04:00", Weekdays: []int{int(time.Sunday)}}

	monday := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if monday.Weekday() != time.Monday {
		t.Fatalf("fixture drift: %s is a %s", monday, monday.Weekday())
	}
	next, ok := window.NextOpen(monday)
	if !ok {
		t.Fatal("a weekly window opens within the horizon")
	}
	want := time.Date(2026, 3, 8, 2, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next open = %s, want %s", next, want)
	}
}

func TestWindowValidate(t *testing.T) {
	if err := (MaintenanceWindow{AlwaysOpen: true}).Validate(); err != nil {
		t.Fatalf("an always-open window is valid: %v", err)
	}
	if err := (MaintenanceWindow{Start: "02:00", End: "04:00", Weekdays: []int{7}}).Validate(); err == nil {
		t.Fatal("weekday 7 is out of range")
	}
	if err := (MaintenanceWindow{Start: "02:00", End: "04:00", Weekdays: []int{-1}}).Validate(); err == nil {
		t.Fatal("weekday -1 is out of range")
	}
	if err := (MaintenanceWindow{Timezone: "Europe/London", Start: "22:00", End: "02:00"}).Validate(); err != nil {
		t.Fatalf("a crossing window in a real zone is valid: %v", err)
	}
}

func TestWindowDescribeNamesTheZone(t *testing.T) {
	plain := MaintenanceWindow{Start: "02:00", End: "04:00"}.Describe()
	if want := "02:00-04:00 UTC, every day"; plain != want {
		t.Fatalf("describe = %q, want %q", plain, want)
	}
	crossing := MaintenanceWindow{
		Timezone: "Europe/London",
		Start:    "22:00",
		End:      "02:00",
		Weekdays: []int{int(time.Saturday), int(time.Sunday)},
	}.Describe()
	want := "22:00-02:00 (crossing midnight) Europe/London, Saturday, Sunday"
	if crossing != want {
		t.Fatalf("describe = %q, want %q", crossing, want)
	}
}
