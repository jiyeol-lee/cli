package gcal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/calendar/v3"
)

type Event struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Start   string `json:"start"`
	End     string `json:"end"`
	AllDay  bool   `json:"all_day"`

	startTime time.Time
	endTime   time.Time
}

type EventSource interface {
	ListEvents(context.Context, string, time.Time, time.Time, string) ([]*calendar.Event, string, error)
}

type GoogleSource struct{ Service *calendar.Service }

func (s GoogleSource) ListEvents(ctx context.Context, calendarID string, start, end time.Time, pageToken string) ([]*calendar.Event, string, error) {
	call := s.Service.Events.List(calendarID).Context(ctx).ShowDeleted(false).SingleEvents(true).OrderBy("startTime").TimeMin(start.Format(time.RFC3339)).TimeMax(end.Format(time.RFC3339))
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	result, err := call.Do()
	if err != nil {
		return nil, "", err
	}
	return result.Items, result.NextPageToken, nil
}

type Calendar struct {
	Source     EventSource
	CalendarID string
	Location   *time.Location
	Now        func() time.Time
}

func (c Calendar) Today(ctx context.Context) ([]Event, error) {
	loc := c.Location
	if loc == nil {
		loc = time.Local
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	now = now.In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)
	id := c.CalendarID
	if id == "" {
		id = "primary"
	}
	var events []Event
	page := ""
	for {
		items, next, err := c.Source.ListEvents(ctx, id, start, end, page)
		if err != nil {
			return nil, fmt.Errorf("list calendar events: %w", err)
		}
		for _, item := range items {
			event, err := normalizeEvent(item, loc)
			if err != nil {
				id := ""
				if item != nil {
					id = item.Id
				}
				return nil, fmt.Errorf("normalize event %q: %w", id, err)
			}
			events = append(events, event)
		}
		if next == "" {
			break
		}
		page = next
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].AllDay != events[j].AllDay {
			return events[i].AllDay
		}
		return events[i].startTime.Before(events[j].startTime)
	})
	return events, nil
}

func normalizeEvent(item *calendar.Event, loc *time.Location) (Event, error) {
	if item == nil || item.Start == nil || item.End == nil {
		return Event{}, fmt.Errorf("missing start or end")
	}
	e := Event{ID: item.Id, Summary: item.Summary}
	if item.Start.Date != "" {
		start, err := time.ParseInLocation(time.DateOnly, item.Start.Date, loc)
		if err != nil {
			return Event{}, err
		}
		end, err := time.ParseInLocation(time.DateOnly, item.End.Date, loc)
		if err != nil {
			return Event{}, err
		}
		e.Start, e.End, e.AllDay, e.startTime, e.endTime = item.Start.Date, item.End.Date, true, start, end
		return e, nil
	}
	start, err := time.Parse(time.RFC3339, item.Start.DateTime)
	if err != nil {
		return Event{}, err
	}
	end, err := time.Parse(time.RFC3339, item.End.DateTime)
	if err != nil {
		return Event{}, err
	}
	e.startTime, e.endTime = start.In(loc), end.In(loc)
	e.Start, e.End = e.startTime.Format(time.RFC3339), e.endTime.Format(time.RFC3339)
	return e, nil
}

func Soon(events []Event, now time.Time) []Event {
	selected := []Event{}
	var earliest time.Time
	for i := range events {
		e := events[i]
		if e.AllDay || !e.startTime.After(now) {
			continue
		}
		if len(selected) == 0 || e.startTime.Before(earliest) {
			earliest = e.startTime
			selected = []Event{e}
			continue
		}
		if e.startTime.Equal(earliest) {
			selected = append(selected, e)
		}
	}
	return selected
}

func InProgress(events []Event, now time.Time) []Event {
	selected := []Event{}
	for i := range events {
		e := events[i]
		if e.AllDay || e.startTime.After(now) || !now.Before(e.endTime) {
			continue
		}
		selected = append(selected, e)
	}
	return selected
}

func WriteEvents(out io.Writer, events []Event, text bool, separator string) error {
	if text {
		if len(events) == 0 {
			_, err := io.WriteString(out, "N/A\n")
			return err
		}
		rendered := make([]string, 0, len(events))
		for _, event := range events {
			if event.AllDay {
				rendered = append(rendered, event.Summary)
				continue
			}
			rendered = append(rendered, fmt.Sprintf("%s (%s - %s)", event.Summary, event.startTime.Format("15:04"), event.endTime.Format("15:04")))
		}
		_, err := fmt.Fprintln(out, strings.Join(rendered, separator))
		return err
	}
	if events == nil {
		events = []Event{}
	}
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(events)
}
