package gcal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"google.golang.org/api/calendar/v3"
)

type fakeSource struct {
	pages [][]*calendar.Event
	calls int
	start time.Time
	end   time.Time
	id    string
}

func (f *fakeSource) ListEvents(_ context.Context, id string, start, end time.Time, page string) ([]*calendar.Event, string, error) {
	f.id, f.start, f.end = id, start, end
	items := f.pages[f.calls]
	f.calls++
	if f.calls < len(f.pages) {
		return items, "next", nil
	}
	return items, "", nil
}

func timed(id, summary string, start, end time.Time) *calendar.Event {
	return &calendar.Event{Id: id, Summary: summary, Start: &calendar.EventDateTime{DateTime: start.Format(time.RFC3339)}, End: &calendar.EventDateTime{DateTime: end.Format(time.RFC3339)}}
}

func TestTodayPaginationAndNormalization(t *testing.T) {
	loc := time.FixedZone("local", 9*60*60)
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, loc)
	source := &fakeSource{pages: [][]*calendar.Event{
		{{Id: "all", Summary: "Holiday", Start: &calendar.EventDateTime{Date: "2026-08-19"}, End: &calendar.EventDateTime{Date: "2026-08-20"}}},
		{timed("timed", "Meeting", now.Add(time.Hour), now.Add(2*time.Hour))},
	}}
	cal := Calendar{Source: source, Location: loc, Now: func() time.Time { return now }}
	events, err := cal.Today(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if source.calls != 2 || source.id != "primary" {
		t.Fatalf("calls=%d id=%q", source.calls, source.id)
	}
	if !source.start.Equal(time.Date(2026, 8, 19, 0, 0, 0, 0, loc)) || !source.end.Equal(time.Date(2026, 8, 20, 0, 0, 0, 0, loc)) {
		t.Fatalf("range %v to %v", source.start, source.end)
	}
	if len(events) != 2 || !events[0].AllDay || events[0].End != "2026-08-20" || events[1].Start != "2026-08-19T11:00:00+09:00" {
		t.Fatalf("events = %#v", events)
	}
}

func TestTodayReturnsErrorForNilEvent(t *testing.T) {
	source := &fakeSource{pages: [][]*calendar.Event{{nil}}}
	cal := Calendar{Source: source, Location: time.UTC}
	if _, err := cal.Today(context.Background()); err == nil {
		t.Fatal("expected error for nil event")
	}
}

func TestInProgressReturnsOverlappingEventsInInputOrder(t *testing.T) {
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{ID: "ended", startTime: now.Add(-time.Hour), endTime: now},
		{ID: "first", startTime: now, endTime: now.Add(time.Hour)},
		{ID: "second", startTime: now.Add(-30 * time.Minute), endTime: now.Add(30 * time.Minute)},
		{ID: "future", startTime: now.Add(time.Minute), endTime: now.Add(time.Hour)},
		{ID: "all", AllDay: true, startTime: now},
	}
	got := InProgress(events, now)
	if len(got) != 2 || got[0].ID != "first" || got[1].ID != "second" {
		t.Fatalf("in progress = %#v", got)
	}
	if got == nil {
		t.Fatal("in progress returned nil")
	}
}

func TestInProgressIncludesStartAndExcludesEnd(t *testing.T) {
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{ID: "at-start", startTime: now, endTime: now.Add(time.Hour)},
		{ID: "at-end", startTime: now.Add(-time.Hour), endTime: now},
	}
	got := InProgress(events, now)
	if len(got) != 1 || got[0].ID != "at-start" {
		t.Fatalf("in progress = %#v", got)
	}
}

func TestSoonReturnsEarliestTiesInInputOrder(t *testing.T) {
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	earliest := now.Add(30 * time.Minute)
	events := []Event{
		{ID: "later", startTime: now.Add(time.Hour)},
		{ID: "first", startTime: earliest},
		{ID: "all", AllDay: true, startTime: earliest},
		{ID: "second", startTime: earliest},
		{ID: "now", startTime: now},
	}
	got := Soon(events, now)
	if len(got) != 2 || got[0].ID != "first" || got[1].ID != "second" {
		t.Fatalf("soon = %#v", got)
	}
}

func TestSoonWithoutTies(t *testing.T) {
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{ID: "first", startTime: now.Add(time.Minute)},
		{ID: "later", startTime: now.Add(time.Hour)},
	}
	got := Soon(events, now)
	if len(got) != 1 || got[0].ID != "first" {
		t.Fatalf("soon = %#v", got)
	}
}

func TestSoonWithoutFutureTimedEvents(t *testing.T) {
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{ID: "now", startTime: now},
		{ID: "past", startTime: now.Add(-time.Minute)},
		{ID: "all", AllDay: true, startTime: now.Add(time.Minute)},
	}
	got := Soon(events, now)
	if len(got) != 0 {
		t.Fatalf("soon = %#v", got)
	}
	if got == nil {
		t.Fatal("soon returned nil")
	}
}

func TestSoonIncludesLegacyWorkingHoursEvent(t *testing.T) {
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	item := timed("legacy", "Formerly marked", now.Add(time.Hour), now.Add(2*time.Hour))
	item.ExtendedProperties = &calendar.EventExtendedProperties{Private: map[string]string{"WORKING_HOURS": "0.000"}}
	event, err := normalizeEvent(item, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if got := Soon([]Event{event}, now); len(got) != 1 || got[0].ID != "legacy" {
		t.Fatalf("soon = %#v", got)
	}
}

func TestOutputFormats(t *testing.T) {
	loc := time.UTC
	events := []Event{
		{ID: "1", Summary: "Call", Start: "2026-08-19T08:30:00Z", End: "2026-08-19T09:00:00Z", startTime: time.Date(2026, 8, 19, 8, 30, 0, 0, loc), endTime: time.Date(2026, 8, 19, 9, 0, 0, 0, loc)},
		{ID: "2", Summary: "Holiday", Start: "2026-08-19", End: "2026-08-20", AllDay: true},
	}
	var out bytes.Buffer
	if err := WriteEvents(&out, events, false, "ignored"); err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || len(decoded[0]) != 5 {
		t.Fatalf("JSON = %s", out.String())
	}
	out.Reset()
	if err := WriteEvents(&out, events, true, "\n"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Call (08:30 - 09:00)\nHoliday\n" {
		t.Fatalf("text = %q", out.String())
	}
	out.Reset()
	if err := WriteEvents(&out, events, true, ","); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Call (08:30 - 09:00),Holiday\n" {
		t.Fatalf("joined text = %q", out.String())
	}
	out.Reset()
	if err := WriteEvents(&out, events, true, ""); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Call (08:30 - 09:00)Holiday\n" {
		t.Fatalf("empty separator text = %q", out.String())
	}
	out.Reset()
	if err := WriteEvents(&out, events[:1], true, ","); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Call (08:30 - 09:00)\n" {
		t.Fatalf("single joined text = %q", out.String())
	}
	out.Reset()
	if err := WriteEvents(&out, events[:1], true, "\n"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Call (08:30 - 09:00)\n" {
		t.Fatalf("single text = %q", out.String())
	}
	out.Reset()
	if err := WriteEvents(&out, nil, true, ","); err != nil {
		t.Fatal(err)
	}
	if out.String() != "N/A\n" {
		t.Fatalf("empty text = %q", out.String())
	}
	out.Reset()
	if err := WriteEvents(&out, nil, false, "ignored"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Fatalf("empty JSON = %q", out.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestWriteEventsReturnsWriterError(t *testing.T) {
	event := Event{Summary: "Call", startTime: time.Date(2026, 8, 19, 8, 30, 0, 0, time.UTC), endTime: time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)}
	if err := WriteEvents(failingWriter{}, []Event{event}, true, "\n"); err == nil {
		t.Fatal("expected writer error")
	}
}
