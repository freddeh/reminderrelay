package model

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// NormalizePriority
// ---------------------------------------------------------------------------

func TestNormalizePriority(t *testing.T) {
	tests := []struct {
		raw  int
		want Priority
	}{
		{0, PriorityNone},
		{1, PriorityHigh},
		{2, PriorityHigh},
		{3, PriorityHigh},
		{4, PriorityHigh},
		{5, PriorityMedium},
		{6, PriorityLow},
		{7, PriorityLow},
		{8, PriorityLow},
		{9, PriorityLow},
		{-1, PriorityNone},
		{10, PriorityNone},
		{100, PriorityNone},
	}
	for _, tt := range tests {
		if got := NormalizePriority(tt.raw); got != tt.want {
			t.Errorf("NormalizePriority(%d) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Priority.String
// ---------------------------------------------------------------------------

func TestPriority_String(t *testing.T) {
	tests := []struct {
		p    Priority
		want string
	}{
		{PriorityNone, "None"},
		{PriorityHigh, "High"},
		{PriorityMedium, "Medium"},
		{PriorityLow, "Low"},
		{Priority(42), "None"}, // unknown value
	}
	for _, tt := range tests {
		if got := tt.p.String(); got != tt.want {
			t.Errorf("Priority(%d).String() = %q, want %q", tt.p, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// ContentHash
// ---------------------------------------------------------------------------

func TestContentHash_Deterministic(t *testing.T) {
	due := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	item := &Item{
		Title:       "Buy milk",
		Description: "Whole milk preferred",
		DueDate:     &due,
		Priority:    PriorityHigh,
		Completed:   false,
	}
	h1 := item.ContentHash()
	h2 := item.ContentHash()
	if h1 != h2 {
		t.Error("ContentHash not deterministic")
	}
}

func TestContentHash_DiffersOnTitleChange(t *testing.T) {
	item := &Item{Title: "Buy milk", Priority: PriorityNone}
	h1 := item.ContentHash()
	item.Title = "Buy bread"
	h2 := item.ContentHash()
	if h1 == h2 {
		t.Error("ContentHash should differ when title changes")
	}
}

func TestContentHash_DiffersOnPriorityChange(t *testing.T) {
	item := &Item{Title: "Task", Priority: PriorityHigh}
	h1 := item.ContentHash()
	item.Priority = PriorityLow
	h2 := item.ContentHash()
	if h1 == h2 {
		t.Error("ContentHash should differ when priority changes")
	}
}

func TestContentHash_DiffersOnCompletedChange(t *testing.T) {
	item := &Item{Title: "Task", Completed: false}
	h1 := item.ContentHash()
	item.Completed = true
	h2 := item.ContentHash()
	if h1 == h2 {
		t.Error("ContentHash should differ when completed changes")
	}
}

func TestContentHash_IgnoresModifiedAt(t *testing.T) {
	item := &Item{Title: "Task", ModifiedAt: time.Now()}
	h1 := item.ContentHash()
	item.ModifiedAt = item.ModifiedAt.Add(time.Hour)
	h2 := item.ContentHash()
	if h1 != h2 {
		t.Error("ContentHash should not change when only ModifiedAt changes")
	}
}

func TestContentHash_NilDueDate(t *testing.T) {
	item := &Item{Title: "No due", DueDate: nil}
	h := item.ContentHash()
	if h == "" {
		t.Error("ContentHash should be non-empty even with nil DueDate")
	}
}

// ---------------------------------------------------------------------------
// CalendarHash
// ---------------------------------------------------------------------------

func TestCalendarHash_IgnoresDescriptionAndPriority(t *testing.T) {
	due := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	a := &Item{Title: "Task", DueDate: &due, Priority: PriorityHigh, Description: "notes A"}
	b := &Item{Title: "Task", DueDate: &due, Priority: PriorityLow, Description: "notes B"}
	if a.CalendarHash() != b.CalendarHash() {
		t.Error("CalendarHash should be unaffected by Description/Priority differences")
	}
}

func TestCalendarHash_DiffersOnDueDateOrCompleted(t *testing.T) {
	due1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	due2 := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

	base := &Item{Title: "Task", DueDate: &due1, Completed: false}
	diffDue := &Item{Title: "Task", DueDate: &due2, Completed: false}
	diffCompleted := &Item{Title: "Task", DueDate: &due1, Completed: true}

	if base.CalendarHash() == diffDue.CalendarHash() {
		t.Error("CalendarHash should differ when DueDate changes")
	}
	if base.CalendarHash() == diffCompleted.CalendarHash() {
		t.Error("CalendarHash should differ when Completed changes")
	}
}

// ---------------------------------------------------------------------------
// Priority prefix encoding / decoding
// ---------------------------------------------------------------------------

func TestEncodePriorityPrefix(t *testing.T) {
	tests := []struct {
		p    Priority
		desc string
		want string
	}{
		{PriorityHigh, "Buy milk", "[High] Buy milk"},
		{PriorityMedium, "Email boss", "[Medium] Email boss"},
		{PriorityLow, "Tidy desk", "[Low] Tidy desk"},
		{PriorityNone, "Whatever", "Whatever"},
		{PriorityHigh, "", "[High] "},
		{PriorityNone, "", ""},
	}
	for _, tt := range tests {
		if got := EncodePriorityPrefix(tt.p, tt.desc); got != tt.want {
			t.Errorf("EncodePriorityPrefix(%v, %q) = %q, want %q", tt.p, tt.desc, got, tt.want)
		}
	}
}

func TestDecodePriorityPrefix(t *testing.T) {
	tests := []struct {
		input    string
		wantP    Priority
		wantDesc string
	}{
		{"[High] Buy milk", PriorityHigh, "Buy milk"},
		{"[Medium] Email boss", PriorityMedium, "Email boss"},
		{"[Low] Tidy desk", PriorityLow, "Tidy desk"},
		{"No prefix here", PriorityNone, "No prefix here"},
		{"[High] ", PriorityHigh, ""},
		{"", PriorityNone, ""},
		// Partial prefix — should NOT match
		{"[High]No space", PriorityNone, "[High]No space"},
	}
	for _, tt := range tests {
		gotP, gotDesc := DecodePriorityPrefix(tt.input)
		if gotP != tt.wantP || gotDesc != tt.wantDesc {
			t.Errorf("DecodePriorityPrefix(%q) = (%v, %q), want (%v, %q)",
				tt.input, gotP, gotDesc, tt.wantP, tt.wantDesc)
		}
	}
}

// ---------------------------------------------------------------------------
// ListMapping YAML (short/long form)
// ---------------------------------------------------------------------------

func TestListMapping_UnmarshalShortForm(t *testing.T) {
	var m ListMapping
	if err := yaml.Unmarshal([]byte(`todo.shopping`), &m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.HAEntity != "todo.shopping" || m.HACalendarEntity != "" {
		t.Errorf("got %+v, want {todo.shopping, \"\"}", m)
	}
}

func TestListMapping_UnmarshalLongForm(t *testing.T) {
	var m ListMapping
	input := "ha_entity: todo.work\nha_calendar_entity: calendar.work_due\n"
	if err := yaml.Unmarshal([]byte(input), &m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.HAEntity != "todo.work" || m.HACalendarEntity != "calendar.work_due" {
		t.Errorf("got %+v, want {todo.work, calendar.work_due}", m)
	}
}

func TestListMapping_MarshalShortFormWhenNoCalendar(t *testing.T) {
	m := ListMapping{HAEntity: "todo.shopping"}
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(out); got != "todo.shopping\n" {
		t.Errorf("got %q, want %q", got, "todo.shopping\n")
	}
}

func TestListMapping_MarshalLongFormWhenCalendarSet(t *testing.T) {
	m := ListMapping{HAEntity: "todo.work", HACalendarEntity: "calendar.work_due"}
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var roundTripped ListMapping
	if err := yaml.Unmarshal(out, &roundTripped); err != nil {
		t.Fatalf("unmarshalling marshalled output: %v", err)
	}
	if roundTripped != m {
		t.Errorf("round-trip = %+v, want %+v", roundTripped, m)
	}
}

func TestPriorityPrefixRoundTrip(t *testing.T) {
	for _, p := range []Priority{PriorityNone, PriorityHigh, PriorityMedium, PriorityLow} {
		desc := "some task description"
		encoded := EncodePriorityPrefix(p, desc)
		gotP, gotDesc := DecodePriorityPrefix(encoded)
		if gotP != p {
			t.Errorf("round-trip priority: got %v, want %v", gotP, p)
		}
		if gotDesc != desc {
			t.Errorf("round-trip description: got %q, want %q", gotDesc, desc)
		}
	}
}
