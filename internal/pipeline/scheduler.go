package pipeline

import (
	"context"
	"sync"
	"time"

	"github.com/luigiazoreng/solarguard/internal/store"
)

// Scheduler runs the pipeline once per local day after 02:00 in each plant's time zone.
type Scheduler struct {
	store *store.Store
	deps  Deps
	mu    sync.Mutex
}

// NewScheduler creates a pipeline scheduler.
func NewScheduler(st *store.Store, deps Deps) *Scheduler {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Scheduler{
		store: st,
		deps:  deps,
	}
}

// CheckAndRunOnce checks all plants sequentially and runs any plant that is past 02:00
// locally and has not yet run for today.
func (s *Scheduler) CheckAndRunOnce(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	plants, err := s.store.ListPlants(ctx)
	if err != nil {
		return err
	}

	now := s.deps.Now()
	for _, p := range plants {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		loc, err := time.LoadLocation(p.Timezone)
		if err != nil {
			continue
		}

		plantNow := now.In(loc)
		if plantNow.Hour() < 2 {
			// Not yet past 02:00 in plant timezone
			continue
		}

		localDate := plantNow.Format(time.DateOnly)
		alreadyRun, err := s.store.HasPipelineRun(ctx, p.ID, localDate)
		if err != nil {
			continue
		}
		if alreadyRun {
			// Already ran today. Run again only if the collector stored readings
			// after that run, for example a catch-up after a restart.
			unchecked, err := s.store.HasUncheckedReadings(ctx, p.ID)
			if err != nil {
				if s.deps.Logger != nil {
					s.deps.Logger.Warn("check unchecked readings", "plant_id", p.ID, "err", err)
				}
				continue
			}
			if !unchecked {
				continue
			}
		}

		if _, err := RunPlant(ctx, s.store, p.ID, s.deps); err != nil && s.deps.Logger != nil {
			s.deps.Logger.Error("pipeline run failed", "plant_id", p.ID, "err", err)
		}
	}
	return nil
}

// Start begins a background ticker checking every 15 minutes until ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	_ = s.CheckAndRunOnce(ctx)

	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.CheckAndRunOnce(ctx)
		}
	}
}
