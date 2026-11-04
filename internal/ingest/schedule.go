package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// Discover enqueues discovery tasks for a source with refresh=true.
func Discover(ctx context.Context, st *store.Store, sourceID int64, conn connector.Connector, now time.Time) error {
	tasks := conn.Discovery()
	for _, t := range tasks {
		if err := st.EnqueueTask(ctx, sourceID, t, now, true); err != nil {
			return fmt.Errorf("enqueue discovery task %s: %w", t.LimitKey, err)
		}
	}
	return nil
}

// ScheduleDay enqueues daily tasks for every plant of the source for the given date.
func ScheduleDay(ctx context.Context, st *store.Store, sourceID int64, conn connector.Connector, date string) error {
	plants, err := st.ListPlantsBySource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("list plants: %w", err)
	}

	for _, p := range plants {
		devices, err := st.ListDevices(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("list devices for plant %d: %w", p.ID, err)
		}
		tasks := conn.DailyTasks(p, devices, date)
		for _, t := range tasks {
			if err := st.EnqueueTask(ctx, sourceID, t, time.Now(), false); err != nil {
				return fmt.Errorf("enqueue daily task %s: %w", t.LimitKey, err)
			}
		}
	}
	return nil
}

// ScheduleCompletedDays checks each plant of the source and enqueues yesterday per plant's time zone
// only once the plant's local time is past 01:00, and never enqueues today.
func ScheduleCompletedDays(ctx context.Context, st *store.Store, sourceID int64, conn connector.Connector, now func() time.Time) error {
	plants, err := st.ListPlantsBySource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("list plants: %w", err)
	}

	currentTime := now()
	for _, p := range plants {
		tz := p.Timezone
		if tz == "" {
			tz = "America/Sao_Paulo"
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			continue
		}

		localNow := currentTime.In(loc)
		if localNow.Hour() < 1 {
			// Not yet past 01:00 in this plant's time zone
			continue
		}

		yesterday := localNow.AddDate(0, 0, -1).Format("2006-01-02")
		devices, err := st.ListDevices(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("list devices for plant %d: %w", p.ID, err)
		}

		tasks := conn.DailyTasks(p, devices, yesterday)
		for _, t := range tasks {
			if err := st.EnqueueTask(ctx, sourceID, t, currentTime, false); err != nil {
				return fmt.Errorf("enqueue completed day task %s: %w", t.LimitKey, err)
			}
		}
	}

	return nil
}

// Backfill enqueues one day per device per date in [from, to] with lower priority than daily tasks.
func Backfill(ctx context.Context, st *store.Store, sourceID int64, conn connector.Connector, plantID int64, from, to string) error {
	startDate, err := time.Parse("2006-01-02", from)
	if err != nil {
		return fmt.Errorf("parse from date %q: %w", from, err)
	}
	endDate, err := time.Parse("2006-01-02", to)
	if err != nil {
		return fmt.Errorf("parse to date %q: %w", to, err)
	}

	plant, err := st.GetPlant(ctx, plantID)
	if err != nil {
		return fmt.Errorf("get plant %d: %w", plantID, err)
	}

	devices, err := st.ListDevices(ctx, plantID)
	if err != nil {
		return fmt.Errorf("list devices for plant %d: %w", plantID, err)
	}

	for cur := startDate; !cur.After(endDate); cur = cur.AddDate(0, 0, 1) {
		dateStr := cur.Format("2006-01-02")
		tasks := conn.DailyTasks(plant, devices, dateStr)
		for _, t := range tasks {
			t.Priority = 0 // Lower priority than daily tasks (priority 10)
			if err := st.EnqueueTask(ctx, sourceID, t, time.Now(), false); err != nil {
				return fmt.Errorf("enqueue backfill task %s: %w", t.LimitKey, err)
			}
		}
	}

	return nil
}
