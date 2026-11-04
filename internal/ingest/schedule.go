package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
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
// If discovery tasks are pending for this source, scheduling is skipped to avoid false synthetic plant devices.
func ScheduleDay(ctx context.Context, st *store.Store, sourceID int64, conn connector.Connector, date string) error {
	pendingDiscovery, err := st.HasPendingDiscoveryTasks(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("check pending discovery: %w", err)
	}
	if pendingDiscovery {
		return nil
	}

	plants, err := st.ListPlantsBySource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("list plants: %w", err)
	}

	for _, p := range plants {
		if p.Timezone == "" {
			log.Printf("skipping plant %s: empty timezone", p.ExternalID)
			continue
		}
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			log.Printf("skipping plant %s: invalid timezone %q: %v", p.ExternalID, p.Timezone, err)
			continue
		}
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

// ScheduleCompletedDays enqueues the last 7 completed local days per plant's time zone
// only once the plant's local time is past 01:00, and never enqueues today.
// Plants with empty or invalid timezones are skipped and logged.
// If discovery tasks are pending for this source, scheduling is skipped.
func ScheduleCompletedDays(ctx context.Context, st *store.Store, sourceID int64, conn connector.Connector, now func() time.Time) error {
	pendingDiscovery, err := st.HasPendingDiscoveryTasks(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("check pending discovery: %w", err)
	}
	if pendingDiscovery {
		return nil
	}

	plants, err := st.ListPlantsBySource(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("list plants: %w", err)
	}

	currentTime := now()
	for _, p := range plants {
		if p.Timezone == "" {
			log.Printf("skipping plant %s: empty timezone", p.ExternalID)
			continue
		}
		loc, err := time.LoadLocation(p.Timezone)
		if err != nil {
			log.Printf("skipping plant %s: invalid timezone %q: %v", p.ExternalID, p.Timezone, err)
			continue
		}

		localNow := currentTime.In(loc)
		if localNow.Hour() < 1 {
			// Not yet past 01:00 in this plant's time zone
			continue
		}

		devices, err := st.ListDevices(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("list devices for plant %d: %w", p.ID, err)
		}

		// Enqueue the last 7 completed local days
		for dayOffset := 1; dayOffset <= 7; dayOffset++ {
			targetDate := localNow.AddDate(0, 0, -dayOffset).Format("2006-01-02")
			tasks := conn.DailyTasks(p, devices, targetDate)
			for _, t := range tasks {
				if err := st.EnqueueTask(ctx, sourceID, t, currentTime, false); err != nil {
					return fmt.Errorf("enqueue completed day task %s: %w", t.LimitKey, err)
				}
			}
		}
	}

	return nil
}

// Backfill enqueues one day per device per date in [from, to] with lower priority than daily tasks.
// Returns an error if the plant has an empty or invalid timezone.
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

	if plant.Timezone == "" {
		return fmt.Errorf("plant %d has empty timezone", plantID)
	}
	if _, err := time.LoadLocation(plant.Timezone); err != nil {
		return fmt.Errorf("plant %d has invalid timezone %q: %w", plantID, plant.Timezone, err)
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

// SchedulerOption configures a Scheduler.
type SchedulerOption func(*Scheduler)

// WithSchedulerNow sets the time provider for Scheduler.
func WithSchedulerNow(now func() time.Time) SchedulerOption {
	return func(s *Scheduler) {
		s.now = now
	}
}

// WithSchedulerLogger sets the logger for Scheduler.
func WithSchedulerLogger(logger *log.Logger) SchedulerOption {
	return func(s *Scheduler) {
		s.logger = logger
	}
}

// Scheduler runs periodic discovery and completed days scheduling for a source.
type Scheduler struct {
	st       *store.Store
	sourceID int64
	conn     connector.Connector
	now      func() time.Time
	logger   *log.Logger
}

// NewScheduler creates a new Scheduler for a source.
func NewScheduler(st *store.Store, sourceID int64, conn connector.Connector, opts ...SchedulerOption) *Scheduler {
	s := &Scheduler{
		st:       st,
		sourceID: sourceID,
		conn:     conn,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Tick runs one iteration of the scheduler: discovery (at most once per UTC day) and ScheduleCompletedDays.
func (s *Scheduler) Tick(ctx context.Context) error {
	currentNow := s.now()
	todayUTC := currentNow.UTC().Format("2006-01-02")

	// Check if discovery ran today using plant/list task's updated_at
	var updatedAtStr string
	err := s.st.DB().QueryRowContext(ctx, `SELECT updated_at FROM fetch_tasks
		WHERE source_id = ? AND task_key LIKE 'plant/list%' AND status = 'done'
		ORDER BY updated_at DESC LIMIT 1`, s.sourceID).Scan(&updatedAtStr)

	ranToday := false
	if err == nil && updatedAtStr != "" {
		if t, parseErr := time.Parse(time.RFC3339, updatedAtStr); parseErr == nil {
			if t.UTC().Format("2006-01-02") == todayUTC {
				ranToday = true
			}
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("query last discovery time: %w", err)
	}

	if !ranToday {
		if err := Discover(ctx, s.st, s.sourceID, s.conn, currentNow); err != nil {
			return fmt.Errorf("run scheduled discovery: %w", err)
		}
	}

	if err := ScheduleCompletedDays(ctx, s.st, s.sourceID, s.conn, s.now); err != nil {
		return fmt.Errorf("run schedule completed days: %w", err)
	}

	return nil
}

// Run executes Tick periodically every 15 minutes until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		if s.logger != nil {
			s.logger.Printf("scheduler tick error for source %d: %v", s.sourceID, err)
		}
	}

	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				if s.logger != nil {
					s.logger.Printf("scheduler tick error for source %d: %v", s.sourceID, err)
				}
			}
		}
	}
}
