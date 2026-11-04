package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

// Runner processes tasks for a single source one by one.
type Runner struct {
	store    *store.Store
	sourceID int64
	conn     connector.Connector
	now      func() time.Time
	logger   *log.Logger
	sleep    func(ctx context.Context, d time.Duration) error
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithNow overrides the current time function.
func WithNow(now func() time.Time) RunnerOption {
	return func(r *Runner) {
		r.now = now
	}
}

// WithLogger sets the logger for the runner.
func WithLogger(logger *log.Logger) RunnerOption {
	return func(r *Runner) {
		r.logger = logger
	}
}

// WithSleep overrides the sleep function.
func WithSleep(sleep func(ctx context.Context, d time.Duration) error) RunnerOption {
	return func(r *Runner) {
		r.sleep = sleep
	}
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// NewRunner creates a new Runner for the given source.
func NewRunner(st *store.Store, sourceID int64, conn connector.Connector, opts ...RunnerOption) *Runner {
	r := &Runner{
		store:    st,
		sourceID: sourceID,
		conn:     conn,
		now:      time.Now,
		sleep:    defaultSleep,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Step chooses and executes one eligible task, returning whether a task was processed.
func (r *Runner) Step(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	now := r.now()
	rec, task, err := r.store.PickNextTask(ctx, r.sourceID, now)
	if err != nil {
		return false, fmt.Errorf("pick next task: %w", err)
	}
	if rec == nil {
		return false, nil
	}

	res, execErr := r.conn.Execute(ctx, *task)
	if ctx.Err() != nil {
		// A cancelled context during Execute must not count an attempt or apply backoff
		return false, ctx.Err()
	}

	handleFailure := func(causeErr error, returnErr bool) (bool, error) {
		newAttempts := rec.Attempts + 1
		failed := (newAttempts >= 8)
		backoffMin := 1 << newAttempts
		backoff := time.Duration(backoffMin) * time.Minute
		if backoff > 6*time.Hour {
			backoff = 6 * time.Hour
		}
		nextNotBefore := r.now().Add(backoff)
		sanitizedErr := sanitizeError(causeErr.Error())

		if err := r.store.MarkTaskFailedOrRetry(context.Background(), rec.ID, newAttempts, sanitizedErr, nextNotBefore, failed, r.now()); err != nil {
			return true, fmt.Errorf("mark task failed or retry: %w", err)
		}
		if returnErr {
			return true, causeErr
		}
		return true, nil
	}

	if execErr != nil {
		if errors.Is(execErr, connector.ErrRateLimited) {
			var rle *connector.RateLimitError
			retryAfter := 5 * time.Minute
			if errors.As(execErr, &rle) && rle.RetryAfter > 0 {
				retryAfter = rle.RetryAfter
			}
			nextAllowed := r.now().Add(retryAfter)
			if err := r.store.MarkTaskRateLimited(ctx, r.sourceID, rec.ID, rec.TaskKey, task.Endpoint, nextAllowed, r.now()); err != nil {
				return true, fmt.Errorf("mark task rate limited: %w", err)
			}
			return true, nil
		}

		return handleFailure(execErr, false)
	}

	// Success: in this order, insert the raw payload, upsert plants, upsert devices,
	// map external IDs to stored IDs, insert readings with RawPayloadID, enqueue followups, mark done.
	paramsJSON, _ := json.Marshal(task.Params)
	payloadID, err := r.store.InsertRawPayload(ctx, r.sourceID, task.Endpoint, string(paramsJSON), res.Raw)
	if err != nil {
		return handleFailure(fmt.Errorf("insert raw payload: %w", err), true)
	}

	plantIDMap := make(map[string]int64)
	for _, p := range res.Plants {
		pStored, err := r.store.UpsertPlant(ctx, domain.Plant{
			SourceID:    r.sourceID,
			ExternalID:  p.ExternalID,
			Name:        p.Name,
			Timezone:    p.Timezone,
			Latitude:    p.Latitude,
			Longitude:   p.Longitude,
			PeakPowerKW: p.PeakPowerKW,
		})
		if err != nil {
			return handleFailure(fmt.Errorf("upsert plant %s: %w", p.ExternalID, err), true)
		}
		plantIDMap[p.ExternalID] = pStored.ID
	}

	deviceIDMap := make(map[string]int64)
	for _, d := range res.Devices {
		pID, ok := plantIDMap[d.PlantExternalID]
		if !ok {
			p, err := r.store.GetPlantByExternalID(ctx, r.sourceID, d.PlantExternalID)
			if err == nil {
				pID = p.ID
				plantIDMap[d.PlantExternalID] = pID
			}
		}
		dStored, err := r.store.UpsertDevice(ctx, domain.Device{
			PlantID:    pID,
			ExternalID: d.ExternalID,
			Name:       d.Name,
			Kind:       d.Kind,
			Online:     d.Online,
		})
		if err != nil {
			return handleFailure(fmt.Errorf("upsert device %s: %w", d.ExternalID, err), true)
		}
		deviceIDMap[d.ExternalID] = dStored.ID
	}

	readingsToInsert := make([]domain.Reading, 0, len(res.Readings))
	for _, rInfo := range res.Readings {
		devID, ok := deviceIDMap[rInfo.DeviceExternalID]
		if !ok {
			pID, okP := plantIDMap[rInfo.PlantExternalID]
			if !okP {
				p, err := r.store.GetPlantByExternalID(ctx, r.sourceID, rInfo.PlantExternalID)
				if err == nil {
					pID = p.ID
					plantIDMap[rInfo.PlantExternalID] = pID
				}
			}
			d, err := r.store.GetDeviceByExternalID(ctx, pID, rInfo.DeviceExternalID)
			if err == nil {
				devID = d.ID
				deviceIDMap[rInfo.DeviceExternalID] = devID
			}
		}

		readingsToInsert = append(readingsToInsert, domain.Reading{
			DeviceID:     devID,
			Metric:       rInfo.Metric,
			PeriodStart:  rInfo.PeriodStart,
			PeriodEnd:    rInfo.PeriodEnd,
			Value:        rInfo.Value,
			RawValue:     rInfo.RawValue,
			RawPayloadID: &payloadID,
		})
	}

	if len(readingsToInsert) > 0 {
		if _, err := r.store.InsertReadings(ctx, readingsToInsert); err != nil {
			return handleFailure(fmt.Errorf("insert readings: %w", err), true)
		}
	}

	for _, followup := range res.Followups {
		if err := r.store.EnqueueTask(ctx, r.sourceID, followup, r.now(), followup.Refreshable); err != nil {
			return handleFailure(fmt.Errorf("enqueue followup: %w", err), true)
		}
	}

	if err := r.store.MarkTaskDone(ctx, rec.ID, r.now()); err != nil {
		return handleFailure(fmt.Errorf("mark task done: %w", err), true)
	}

	return true, nil
}

// Run continuously processes tasks until the context is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		processed, err := r.Step(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			if r.logger != nil {
				r.logger.Printf("runner error source=%d: %v", r.sourceID, err)
			}
			// When Step returns an error, sleep before next loop to avoid spinning
			if sleepErr := r.sleep(ctx, time.Second); sleepErr != nil {
				return sleepErr
			}
			continue
		}

		if !processed {
			nextDue, err := r.store.NextDueTime(ctx, r.sourceID, r.now())
			wait := time.Minute
			if err == nil && nextDue != nil {
				dur := nextDue.Sub(r.now())
				if dur > 0 && dur < wait {
					wait = dur
				}
			}
			if err := r.sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
}

func sanitizeError(s string) string {
	// Guard against secrets in error strings
	if idx := strings.Index(s, "token="); idx != -1 {
		return s[:idx] + "token=[REDACTED]"
	}
	return s
}
