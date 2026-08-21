package gcal

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestSoonAndInProgressBoundaries(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, loc)
	events := []Event{
		{ID: "ended", startTime: now.Add(-time.Hour), endTime: now},
		{ID: "active", startTime: now, endTime: now.Add(time.Hour)},
		{ID: "future-work", startTime: now.Add(30 * time.Minute), endTime: now.Add(time.Hour)},
		{ID: "future", startTime: now.Add(time.Hour), endTime: now.Add(2 * time.Hour)},
		{ID: "all", AllDay: true, startTime: now},
	}
	if got := InProgress(events, now); len(got) != 1 || got[0].ID != "active" {
		t.Fatalf("in progress = %#v", got)
	}
	if got := Soon(events, now); len(got) != 1 || got[0].ID != "future-work" {
		t.Fatalf("soon = %#v", got)
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
	events := []Event{{ID: "1", Summary: "Call", Start: "2026-08-19T08:30:00Z", End: "2026-08-19T09:00:00Z", startTime: time.Date(2026, 8, 19, 8, 30, 0, 0, loc), endTime: time.Date(2026, 8, 19, 9, 0, 0, 0, loc)}}
	var out bytes.Buffer
	if err := WriteEvents(&out, events, false); err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || len(decoded[0]) != 5 {
		t.Fatalf("JSON = %s", out.String())
	}
	out.Reset()
	if err := WriteEvents(&out, events, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Call (08:30 - 09:00)\n" {
		t.Fatalf("text = %q", out.String())
	}
	out.Reset()
	if err := WriteEvents(&out, nil, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Fatalf("empty JSON = %q", out.String())
	}
}
