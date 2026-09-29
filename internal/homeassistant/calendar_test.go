package homeassistant

import (
	"testing"
	"time"

	haclient "github.com/mkelcik/go-ha-client/v2"

	"github.com/njoerd114/reminderrelay/internal/model"
)

// ---------------------------------------------------------------------------
// calendarEventPayload
// ---------------------------------------------------------------------------

func TestCalendarEventPayload_AllDay_UsesLocalCalendarDay(t *testing.T) {
	withLocalTimezone(t, "Europe/Berlin")

	localMidnight := time.Date(2026, 9, 29, 0, 0, 0, 0, time.Local)
	asUTC := localMidnight.UTC() // what a date-only DueDate actually looks like — see convert_test.go
	item := &model.Item{Title: "Buy milk", DueDate: &asUTC}

	event := calendarEventPayload(item)

	if event["dtstart"] != "2026-09-29" {
		t.Errorf("dtstart = %v, want %q (local calendar day, not the UTC one)", event["dtstart"], "2026-09-29")
	}
	if event["dtend"] != "2026-09-30" {
		t.Errorf("dtend = %v, want %q (exclusive end, one local day later)", event["dtend"], "2026-09-30")
	}
}

func TestCalendarEventPayload_Timed_PreservesInstant(t *testing.T) {
	due := time.Date(2026, 9, 29, 14, 30, 0, 0, time.Local)
	item := &model.Item{Title: "Buy milk", DueDate: &due}

	event := calendarEventPayload(item)

	gotStart, err := time.Parse(time.RFC3339, event["dtstart"].(string))
	if err != nil {
		t.Fatalf("dtstart not a valid RFC3339 timestamp: %v", err)
	}
	if !gotStart.Equal(due) {
		t.Errorf("dtstart = %v, want the same instant as %v", gotStart, due)
	}

	gotEnd, err := time.Parse(time.RFC3339, event["dtend"].(string))
	if err != nil {
		t.Fatalf("dtend not a valid RFC3339 timestamp: %v", err)
	}
	if !gotEnd.Equal(due.Add(defaultCalendarEventDuration)) {
		t.Errorf("dtend = %v, want dtstart + %v", gotEnd, defaultCalendarEventDuration)
	}
}

func TestCalendarEventPayload_CompletedTitlePrefix(t *testing.T) {
	due := time.Date(2026, 9, 29, 0, 0, 0, 0, time.Local)
	item := &model.Item{Title: "Buy milk", DueDate: &due, Completed: true}

	event := calendarEventPayload(item)

	if event["summary"] != "[Done] Buy milk" {
		t.Errorf("summary = %v, want %q", event["summary"], "[Done] Buy milk")
	}
}

// ---------------------------------------------------------------------------
// parseCalendarEventTime
// ---------------------------------------------------------------------------

func TestParseCalendarEventTime_AllDay_ProducesLocalMidnight(t *testing.T) {
	withLocalTimezone(t, "America/New_York")

	got, err := parseCalendarEventTime(haclient.CalendarEventTime{Date: "2026-09-29"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want a parsed time")
	}
	if got.Year() != 2026 || got.Month() != time.September || got.Day() != 29 {
		t.Errorf("parsed date = %v, want calendar day 2026-09-29", got)
	}
	if got.Hour() != 0 || got.Minute() != 0 {
		t.Errorf("parsed date = %v, want local midnight", got)
	}
}

func TestParseCalendarEventTime_AllDay_AgreesWithParseDue(t *testing.T) {
	withLocalTimezone(t, "Europe/Berlin")

	fromCalendar, err := parseCalendarEventTime(haclient.CalendarEventTime{Date: "2026-09-29"})
	if err != nil {
		t.Fatalf("parseCalendarEventTime: %v", err)
	}
	fromTodo, err := parseDue("2026-09-29")
	if err != nil {
		t.Fatalf("parseDue: %v", err)
	}

	// The 3-way due-date merge (see package sync) compares these two
	// sources by instant — they must agree on the same date-only due date,
	// or the merge would see a phantom calendar-side change every pass.
	if !fromCalendar.Equal(fromTodo) {
		t.Errorf("parseCalendarEventTime = %v, parseDue = %v, want equal instants", fromCalendar, fromTodo)
	}
}

func TestParseCalendarEventTime_DateTime(t *testing.T) {
	got, err := parseCalendarEventTime(haclient.CalendarEventTime{DateTime: "2026-09-29T14:30:00+02:00"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-09-29T14:30:00+02:00")
	if !got.Equal(want) {
		t.Errorf("parsed = %v, want %v", got, want)
	}
}

func TestParseCalendarEventTime_Neither_ReturnsError(t *testing.T) {
	_, err := parseCalendarEventTime(haclient.CalendarEventTime{})
	if err == nil {
		t.Error("expected error when neither Date nor DateTime is set")
	}
}
