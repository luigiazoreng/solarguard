package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/connector/growatt"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
)

type mockConnector struct {
	kind       string
	discovery  func() []connector.Task
	dailyTasks func(plant domain.Plant, devices []domain.Device, date string) []connector.Task
	execute    func(ctx context.Context, task connector.Task) (connector.Result, error)
	calls      []connector.Task
	mu         sync.Mutex
}

func (m *mockConnector) Kind() string {
	if m.kind == "" {
		return "growatt"
	}
	return m.kind
}

func (m *mockConnector) Discovery() []connector.Task {
	if m.discovery != nil {
		return m.discovery()
	}
	return nil
}

func (m *mockConnector) DailyTasks(plant domain.Plant, devices []domain.Device, date string) []connector.Task {
	if m.dailyTasks != nil {
		return m.dailyTasks(plant, devices, date)
	}
	return nil
}

func (m *mockConnector) Execute(ctx context.Context, task connector.Task) (connector.Result, error) {
	m.mu.Lock()
	m.calls = append(m.calls, task)
	m.mu.Unlock()
	if m.execute != nil {
		return m.execute(ctx, task)
	}
	return connector.Result{Raw: []byte(`{"error_code": 0}`)}, nil
}

func (m *mockConnector) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func newTestStore(t *testing.T, now func() time.Time) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.SetNow(now)
	t.Cleanup(func() { st.Close() })
	return st
}

// 1. rate limit: a 10012 response keeps the task pending, sets next_allowed_at, and does not count an attempt.
func TestRateLimit_10012KeepsPendingAndDoesNotCountAttempt(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	task := connector.Task{
		Endpoint:    "plant/list",
		Params:      map[string]string{"page": "1", "perpage": "100"},
		LimitKey:    "plant/list?page=1&perpage=100",
		MinInterval: 5 * time.Minute,
	}

	if err := st.EnqueueTask(ctx, src.ID, task, currentTime, false); err != nil {
		t.Fatal(err)
	}

	mock := &mockConnector{
		execute: func(ctx context.Context, t connector.Task) (connector.Result, error) {
			return connector.Result{}, &connector.RateLimitError{RetryAfter: 5 * time.Minute}
		},
	}

	runner := NewRunner(st, src.ID, mock, WithNow(now))

	processed, err := runner.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("expected runner to process task")
	}

	// Verify task in DB: status is pending, attempts is 0, not_before is currentTime + 5m
	stats, err := st.GetQueueStats(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 || stats.Failed != 0 || stats.Done != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if stats.NextDue == nil || !stats.NextDue.Equal(currentTime.Add(5*time.Minute)) {
		t.Fatalf("next_due = %v, want %v", stats.NextDue, currentTime.Add(5*time.Minute))
	}
}

// 2. two plants with the same endpoint both run with no wait, because the LimitKey differs; the same LimitKey twice waits MinInterval.
func TestLimitKeys_DifferentRunImmediatelySameWaitsMinInterval(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	taskP1 := connector.Task{
		Endpoint:    "plant/details",
		Params:      map[string]string{"plant_id": "1"},
		LimitKey:    "plant/details?plant_id=1",
		MinInterval: 5 * time.Minute,
		Priority:    10,
	}
	taskP2 := connector.Task{
		Endpoint:    "plant/details",
		Params:      map[string]string{"plant_id": "2"},
		LimitKey:    "plant/details?plant_id=2",
		MinInterval: 5 * time.Minute,
		Priority:    10,
	}

	if err := st.EnqueueTask(ctx, src.ID, taskP1, currentTime, false); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueTask(ctx, src.ID, taskP2, currentTime, false); err != nil {
		t.Fatal(err)
	}

	mock := &mockConnector{}
	runner := NewRunner(st, src.ID, mock, WithNow(now))

	// Step 1: P1 runs
	proc1, err := runner.Step(ctx)
	if err != nil || !proc1 {
		t.Fatalf("step 1: proc=%v err=%v", proc1, err)
	}

	// Step 2: P2 runs immediately with NO wait!
	proc2, err := runner.Step(ctx)
	if err != nil || !proc2 {
		t.Fatalf("step 2: proc=%v err=%v", proc2, err)
	}
	if mock.CallCount() != 2 {
		t.Fatalf("expected 2 calls, got %d", mock.CallCount())
	}

	// Now re-enqueue P1 with refresh=true
	if err := st.EnqueueTask(ctx, src.ID, taskP1, currentTime, true); err != nil {
		t.Fatal(err)
	}

	// At currentTime, P1 cannot run because MinInterval (5m) has not elapsed!
	proc3, err := runner.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if proc3 {
		t.Fatal("expected P1 to wait MinInterval, but it ran early")
	}

	// Advance time by 5 minutes
	currentTime = currentTime.Add(5 * time.Minute)
	proc4, err := runner.Step(ctx)
	if err != nil || !proc4 {
		t.Fatalf("step 4: proc=%v err=%v", proc4, err)
	}
	if mock.CallCount() != 3 {
		t.Fatalf("expected 3 calls, got %d", mock.CallCount())
	}
}

// 3. restart: a new Runner on the same database respects the persisted next_allowed_at and does not call the server early.
func TestRestart_RespectsPersistedNextAllowedAt(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	dbPath := filepath.Join(t.TempDir(), "restart.db")
	st1, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st1.SetNow(now)
	ctx := context.Background()

	src, err := st1.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	task := connector.Task{
		Endpoint:    "plant/list",
		Params:      map[string]string{"page": "1", "perpage": "100"},
		LimitKey:    "plant/list?page=1&perpage=100",
		MinInterval: 5 * time.Minute,
	}

	if err := st1.EnqueueTask(ctx, src.ID, task, currentTime, false); err != nil {
		t.Fatal(err)
	}

	mock1 := &mockConnector{}
	runner1 := NewRunner(st1, src.ID, mock1, WithNow(now))

	// First execution at 10:00:00
	if proc, err := runner1.Step(ctx); err != nil || !proc {
		t.Fatalf("runner1 step: proc=%v err=%v", proc, err)
	}
	st1.Close()

	// New process / new Runner on same database
	st2, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st2.SetNow(now)
	defer st2.Close()

	// Refresh task to simulate next daily run or re-enqueue
	if err := st2.EnqueueTask(ctx, src.ID, task, currentTime, true); err != nil {
		t.Fatal(err)
	}

	mock2 := &mockConnector{}
	runner2 := NewRunner(st2, src.ID, mock2, WithNow(now))

	// Only 2 minutes later (10:02:00)
	currentTime = currentTime.Add(2 * time.Minute)
	proc, err := runner2.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if proc {
		t.Fatal("runner2 should not have run before next_allowed_at")
	}
	if mock2.CallCount() != 0 {
		t.Fatalf("expected 0 calls on restart before interval, got %d", mock2.CallCount())
	}

	// At 10:05:00 (5 minutes after initial call)
	currentTime = currentTime.Add(3 * time.Minute)
	proc, err = runner2.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("runner2 step at 10:05: proc=%v err=%v", proc, err)
	}
	if mock2.CallCount() != 1 {
		t.Fatalf("expected 1 call after interval, got %d", mock2.CallCount())
	}
}

// 4. a daily budget of 10 stops at 10 and continues on the next UTC day, without blocking other tasks.
func TestDailyBudget_StopsAt10AndContinuesNextDayWithoutBlockingOthers(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	mock := &mockConnector{}
	runner := NewRunner(st, src.ID, mock, WithNow(now))

	// Enqueue 12 tasks with budget 10 (each with different LimitKey so rate limit doesn't interfere)
	for i := 1; i <= 12; i++ {
		tKey := fmt.Sprintf("plant/list?page=%d", i)
		tk := connector.Task{
			Endpoint:       "plant/list",
			Params:         map[string]string{"page": fmt.Sprintf("%d", i)},
			LimitKey:       tKey,
			DailyBudgetKey: "plant/list",
			DailyBudget:    10,
			Priority:       10,
		}
		if err := st.EnqueueTask(ctx, src.ID, tk, currentTime, false); err != nil {
			t.Fatal(err)
		}
	}

	// Enqueue 1 unbudgeted task
	otherTask := connector.Task{
		Endpoint: "plant/details",
		Params:   map[string]string{"plant_id": "99"},
		LimitKey: "plant/details?plant_id=99",
		Priority: 5, // lower priority than budgeted tasks
	}
	if err := st.EnqueueTask(ctx, src.ID, otherTask, currentTime, false); err != nil {
		t.Fatal(err)
	}

	// First 10 budgeted tasks should run
	for i := 1; i <= 10; i++ {
		proc, err := runner.Step(ctx)
		if err != nil || !proc {
			t.Fatalf("step %d: proc=%v err=%v", i, proc, err)
		}
	}

	// 11th budgeted task should be skipped because budget is exhausted,
	// BUT otherTask should run instead!
	proc, err := runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("other task should run: proc=%v err=%v", proc, err)
	}

	// Next step: only remaining 2 budgeted tasks, budget still exhausted, nothing else due
	proc, err = runner.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if proc {
		t.Fatal("expected no tasks to run when budget is exhausted")
	}

	// Advance time to next UTC day (00:00:01 UTC on Nov 5)
	currentTime = time.Date(2026, 11, 5, 0, 0, 1, 0, time.UTC)
	proc, err = runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("task 11 should run on next UTC day: proc=%v err=%v", proc, err)
	}
}

// 5. permanent failure after 8 attempts does not block others; backoff times are correct.
func TestPermanentFailure_After8AttemptsDoesNotBlockOthers(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	failTask := connector.Task{
		Endpoint: "bad/call",
		LimitKey: "bad/call",
		Priority: 10,
	}
	if err := st.EnqueueTask(ctx, src.ID, failTask, currentTime, false); err != nil {
		t.Fatal(err)
	}

	otherTask := connector.Task{
		Endpoint: "good/call",
		LimitKey: "good/call",
		Priority: 5,
	}
	if err := st.EnqueueTask(ctx, src.ID, otherTask, currentTime, false); err != nil {
		t.Fatal(err)
	}

	mock := &mockConnector{
		execute: func(ctx context.Context, task connector.Task) (connector.Result, error) {
			if task.Endpoint == "bad/call" {
				return connector.Result{}, errors.New("500 internal server error")
			}
			return connector.Result{Raw: []byte(`{"error_code": 0}`)}, nil
		},
	}
	runner := NewRunner(st, src.ID, mock, WithNow(now))

	// Expected backoffs: 2^attempts minutes: 2, 4, 8, 16, 32, 64, 128
	expectedBackoffs := []time.Duration{
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		32 * time.Minute,
		64 * time.Minute,
		128 * time.Minute,
	}

	for i := 1; i <= 7; i++ {
		proc, err := runner.Step(ctx)
		if err != nil || !proc {
			t.Fatalf("attempt %d: proc=%v err=%v", i, proc, err)
		}

		var nbStr string
		var attempts int
		var status string
		err = st.DB().QueryRowContext(ctx, `SELECT not_before, attempts, status FROM fetch_tasks WHERE task_key = 'bad/call'`).
			Scan(&nbStr, &attempts, &status)
		if err != nil {
			t.Fatal(err)
		}
		if attempts != i {
			t.Fatalf("attempt %d: got attempts=%d", i, attempts)
		}
		if status != "pending" {
			t.Fatalf("attempt %d: got status=%s, want pending", i, status)
		}

		expectedNext := currentTime.Add(expectedBackoffs[i-1])
		nbTime, err := time.Parse(time.RFC3339, nbStr)
		if err != nil {
			t.Fatal(err)
		}
		if !nbTime.Equal(expectedNext) {
			t.Fatalf("attempt %d not_before = %v, want %v", i, nbTime, expectedNext)
		}

		// Advance clock to next due time
		currentTime = expectedNext
	}

	// 8th attempt: permanent failure
	proc, err := runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("attempt 8: proc=%v err=%v", proc, err)
	}

	stats, err := st.GetQueueStats(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 1 {
		t.Fatalf("expected 1 failed task after 8 attempts, got %d", stats.Failed)
	}
	if len(stats.FailedTasks) != 1 || stats.FailedTasks[0].LastError == "" {
		t.Fatalf("expected last_error recorded for failed task")
	}

	// The permanently failed task never blocks other tasks: otherTask must run now!
	proc, err = runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("other task should run: proc=%v err=%v", proc, err)
	}
}

// 6. ScheduleCompletedDays: two plants in different time zones; never today; not before 01:00 local.
func TestScheduleCompletedDays(t *testing.T) {
	// Base UTC time: 2026-11-04 03:30:00 UTC
	// Plant 1: America/Sao_Paulo (UTC-3) -> local time is 2026-11-04 00:30:00 (before 01:00!). Must NOT enqueue.
	// Plant 2: Asia/Tokyo (UTC+9) -> local time is 2026-11-04 12:30:00 (past 01:00!). Must enqueue yesterday (2026-11-03).
	currentTime := time.Date(2026, 11, 4, 3, 30, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	pSP, err := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P_SP", Name: "SP Plant", Timezone: "America/Sao_Paulo"})
	if err != nil {
		t.Fatal(err)
	}
	pTYO, err := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P_TYO", Name: "Tokyo Plant", Timezone: "Asia/Tokyo"})
	if err != nil {
		t.Fatal(err)
	}

	// Add devices to both
	_, err = st.UpsertDevice(ctx, domain.Device{PlantID: pSP.ID, ExternalID: "INV_SP", Name: "Inv SP"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpsertDevice(ctx, domain.Device{PlantID: pTYO.ID, ExternalID: "INV_TYO", Name: "Inv TYO"})
	if err != nil {
		t.Fatal(err)
	}

	conn := growatt.New("https://example.com", "tok", nil)

	// Run ScheduleCompletedDays at 03:30 UTC
	if err := ScheduleCompletedDays(ctx, st, src.ID, conn, now); err != nil {
		t.Fatal(err)
	}

	stats, err := st.GetQueueStats(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Only Tokyo should have 7 tasks enqueued (7 completed days)
	if stats.Pending != 7 {
		t.Fatalf("expected 7 tasks enqueued (Tokyo only), got %d", stats.Pending)
	}

	// Advance UTC time to 04:30:00 UTC
	// Sao Paulo local time is now 01:30:00 (past 01:00!). Now Sao Paulo gets 7 completed days enqueued!
	currentTime = time.Date(2026, 11, 4, 4, 30, 0, 0, time.UTC)
	if err := ScheduleCompletedDays(ctx, st, src.ID, conn, now); err != nil {
		t.Fatal(err)
	}

	stats, err = st.GetQueueStats(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Now both Tokyo and SP have tasks enqueued (total 14)
	if stats.Pending != 14 {
		t.Fatalf("expected 14 tasks enqueued after SP reaches 01:30, got %d", stats.Pending)
	}
}

// 7. idempotent enqueue; idempotent readings on re-execution.
func TestIdempotentEnqueueAndReadings(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	task := connector.Task{
		Endpoint: "device/inverter/day_energy",
		Params: map[string]string{
			"device_sn": "INV1",
			"date":      "2026-11-04",
			"plant_id":  "P1",
			"timezone":  "America/Sao_Paulo",
		},
		LimitKey: "device/inverter/day_energy?date=2026-11-04&device_sn=INV1&plant_id=P1&timezone=America/Sao_Paulo",
	}

	// Enqueue twice with refresh=false
	if err := st.EnqueueTask(ctx, src.ID, task, currentTime, false); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueTask(ctx, src.ID, task, currentTime, false); err != nil {
		t.Fatal(err)
	}

	stats, _ := st.GetQueueStats(ctx, src.ID)
	if stats.Pending != 1 {
		t.Fatalf("expected 1 task, got %d", stats.Pending)
	}

	start, end, _ := domain.LocalDay("2026-11-04", "America/Sao_Paulo")
	val := 42.5
	mock := &mockConnector{
		execute: func(ctx context.Context, t connector.Task) (connector.Result, error) {
			return connector.Result{
				Plants:  []connector.PlantInfo{{ExternalID: "P1", Name: "Planta 1", Timezone: "America/Sao_Paulo"}},
				Devices: []connector.DeviceInfo{{PlantExternalID: "P1", ExternalID: "INV1", Name: "Inversor 1"}},
				Readings: []connector.ReadingInfo{
					{
						PlantExternalID:  "P1",
						DeviceExternalID: "INV1",
						Metric:           domain.MetricEnergyKWh,
						PeriodStart:      start,
						PeriodEnd:        end,
						Value:            &val,
						RawValue:         "42.5",
					},
				},
				Raw: []byte(`{"error_code": 0, "device_sn": "INV1", "data": "42.5"}`),
			}, nil
		},
	}

	runner := NewRunner(st, src.ID, mock, WithNow(now))

	// Execute task once
	proc, err := runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("step 1: proc=%v err=%v", proc, err)
	}

	// Verify reading inserted
	dev, err := st.GetDeviceByExternalID(ctx, 1, "INV1")
	if err != nil {
		t.Fatal(err)
	}
	readings, err := st.ListReadings(ctx, store.ReadingFilter{
		DeviceID: dev.ID,
		Metric:   domain.MetricEnergyKWh,
		From:     start,
		To:       end,
	})
	if err != nil || len(readings) != 1 {
		t.Fatalf("readings=%d, err=%v", len(readings), err)
	}

	// Re-enqueue task with refresh=true and execute again (advancing clock by MinInterval)
	currentTime = currentTime.Add(5 * time.Minute)
	if err := st.EnqueueTask(ctx, src.ID, task, currentTime, true); err != nil {
		t.Fatal(err)
	}

	proc, err = runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("step 2: proc=%v err=%v", proc, err)
	}

	// Readings count must still be 1 (immutable measurement, harmless re-execution)
	readings, err = st.ListReadings(ctx, store.ReadingFilter{
		DeviceID: dev.ID,
		Metric:   domain.MetricEnergyKWh,
		From:     start,
		To:       end,
	})
	if err != nil || len(readings) != 1 {
		t.Fatalf("readings after re-execution=%d, err=%v", len(readings), err)
	}
}

// 8. a plant with no devices produces the synthetic plant device.
func TestSyntheticPlantDevice(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	// Plant has no devices
	p, err := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P_NODEV", Name: "Solo Plant", Timezone: "America/Sao_Paulo"})
	if err != nil {
		t.Fatal(err)
	}

	conn := growatt.New("https://example.com", "tok", nil)
	dailyTasks := conn.DailyTasks(p, nil, "2026-11-04")
	if len(dailyTasks) != 1 || dailyTasks[0].Endpoint != "plant/energy" {
		t.Fatalf("unexpected daily tasks: %+v", dailyTasks)
	}

	if err := st.EnqueueTask(ctx, src.ID, dailyTasks[0], currentTime, false); err != nil {
		t.Fatal(err)
	}

	start, end, _ := domain.LocalDay("2026-11-04", "America/Sao_Paulo")
	val := 105.8
	mock := &mockConnector{
		execute: func(ctx context.Context, t connector.Task) (connector.Result, error) {
			return connector.Result{
				Devices: []connector.DeviceInfo{
					{
						PlantExternalID: "P_NODEV",
						ExternalID:      "plant:P_NODEV",
						Name:            "Plant P_NODEV",
						Kind:            "plant",
					},
				},
				Readings: []connector.ReadingInfo{
					{
						PlantExternalID:  "P_NODEV",
						DeviceExternalID: "plant:P_NODEV",
						Metric:           domain.MetricEnergyKWh,
						PeriodStart:      start,
						PeriodEnd:        end,
						Value:            &val,
						RawValue:         "105.8",
					},
				},
				Raw: []byte(`{"error_code": 0, "data": {"energys": [{"date": "2026-11-04", "energy": "105.8"}]}}`),
			}, nil
		},
	}

	runner := NewRunner(st, src.ID, mock, WithNow(now))
	proc, err := runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("step: proc=%v err=%v", proc, err)
	}

	// Verify synthetic device was created
	dev, err := st.GetDeviceByExternalID(ctx, p.ID, "plant:P_NODEV")
	if err != nil {
		t.Fatalf("synthetic device not found: %v", err)
	}
	if dev.Kind != "plant" {
		t.Fatalf("synthetic device kind = %s, want plant", dev.Kind)
	}

	// Verify reading was saved for this device
	readings, err := st.ListReadings(ctx, store.ReadingFilter{
		DeviceID: dev.ID,
		Metric:   domain.MetricEnergyKWh,
		From:     start,
		To:       end,
	})
	if err != nil || len(readings) != 1 || readings[0].RawValue != "105.8" {
		t.Fatalf("readings = %+v, err = %v", readings, err)
	}
}

// 9. a missing token env var skips one source and runs the other.
func TestMissingTokenEnvVar_SkipsOneAndRunsTheOther(t *testing.T) {
	st := newTestStore(t, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	os.Setenv("TOKEN_SRC_A", "valid-secret-token")
	os.Unsetenv("TOKEN_SRC_B")
	defer os.Unsetenv("TOKEN_SRC_A")

	srcA, err := st.EnsureSource(ctx, domain.Source{
		Kind:          "growatt",
		Name:          "sourceA",
		CredentialEnv: "TOKEN_SRC_A",
		ConfigJSON:    `{"base_url": "https://example.com"}`,
		Enabled:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = st.EnsureSource(ctx, domain.Source{
		Kind:          "growatt",
		Name:          "sourceB",
		CredentialEnv: "TOKEN_SRC_B",
		ConfigJSON:    `{"base_url": "https://example.com"}`,
		Enabled:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Enqueue a task for source A
	tA := connector.Task{Endpoint: "plant/list", LimitKey: "plant/list?page=1"}
	if err := st.EnqueueTask(ctx, srcA.ID, tA, time.Now(), false); err != nil {
		t.Fatal(err)
	}

	logger := log.New(io.Discard, "", 0)

	// StartAll runs runners in background goroutines
	err = NewManager(ctx, st, nil, logger).StartAll()
	if err != nil {
		t.Fatal(err)
	}

	// Wait briefly and cancel context to check clean shutdown and that Source A ran
	time.Sleep(50 * time.Millisecond)
	cancel()
}

// 10. Runner stops promptly when context is cancelled (race detector check).
func TestRunnerStopsOnContextCancel(t *testing.T) {
	st := newTestStore(t, time.Now)
	ctx, cancel := context.WithCancel(context.Background())

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	mock := &mockConnector{}
	runner := NewRunner(st, src.ID, mock)

	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not exit within 2 seconds")
	}
}

func TestRateLimit_10012CoolsDownWholeEndpoint(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	// Enqueue multiple day_energy tasks for different dates
	for i := 1; i <= 5; i++ {
		date := fmt.Sprintf("2026-11-%02d", i)
		tKey := fmt.Sprintf("device/inverter/day_energy?date=%s&device_sn=INV1", date)
		tk := connector.Task{
			Endpoint: "device/inverter/day_energy",
			Params:   map[string]string{"device_sn": "INV1", "date": date},
			LimitKey: tKey,
			Priority: 10,
		}
		if err := st.EnqueueTask(ctx, src.ID, tk, currentTime, false); err != nil {
			t.Fatal(err)
		}
	}

	// Enqueue a different endpoint: plant/details
	pDetailsTask := connector.Task{
		Endpoint: "plant/details",
		Params:   map[string]string{"plant_id": "1"},
		LimitKey: "plant/details?plant_id=1",
		Priority: 5,
	}
	if err := st.EnqueueTask(ctx, src.ID, pDetailsTask, currentTime, false); err != nil {
		t.Fatal(err)
	}

	dayEnergyCalls := 0
	plantDetailsCalls := 0
	mock := &mockConnector{
		execute: func(ctx context.Context, task connector.Task) (connector.Result, error) {
			if task.Endpoint == "device/inverter/day_energy" {
				dayEnergyCalls++
				return connector.Result{}, &connector.RateLimitError{RetryAfter: 5 * time.Minute}
			}
			if task.Endpoint == "plant/details" {
				plantDetailsCalls++
				return connector.Result{Raw: []byte(`{"error_code": 0}`)}, nil
			}
			return connector.Result{Raw: []byte(`{"error_code": 0}`)}, nil
		},
	}

	runner := NewRunner(st, src.ID, mock, WithNow(now))

	// Step 1: executes first day_energy, gets 10012
	proc1, err := runner.Step(ctx)
	if err != nil || !proc1 {
		t.Fatalf("proc1=%v, err=%v", proc1, err)
	}

	// Step 2: during cooldown, remaining day_energy tasks must NOT run, but plant/details SHOULD run!
	proc2, err := runner.Step(ctx)
	if err != nil || !proc2 {
		t.Fatalf("proc2=%v, err=%v", proc2, err)
	}
	if plantDetailsCalls != 1 {
		t.Fatalf("expected plant/details to run, got %d", plantDetailsCalls)
	}
	if dayEnergyCalls != 1 {
		t.Fatalf("expected exactly 1 call to day_energy during cooldown across pending dates, got %d", dayEnergyCalls)
	}

	// Step 3: further steps before cooldown end should do nothing
	proc3, err := runner.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if proc3 {
		t.Fatal("expected no tasks to run during cooldown")
	}

	// Advance time by 5 minutes: day_energy tasks should resume!
	currentTime = currentTime.Add(5 * time.Minute)
	proc4, err := runner.Step(ctx)
	if err != nil || !proc4 {
		t.Fatalf("proc4=%v, err=%v", proc4, err)
	}
	if dayEnergyCalls != 2 {
		t.Fatalf("expected day_energy calls to resume after cooldown, got %d calls", dayEnergyCalls)
	}
}

func TestPlantListRefreshKeepsTimezone(t *testing.T) {
	st := newTestStore(t, time.Now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	p, err := st.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "P1",
		Name:       "Minha Usina",
		Timezone:   "America/Sao_Paulo",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Plant list refresh has empty timezone and empty name
	_, err = st.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: "P1",
		Name:       "",
		Timezone:   "",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.GetPlant(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Timezone != "America/Sao_Paulo" {
		t.Fatalf("timezone was erased: got %q, want %q", got.Timezone, "America/Sao_Paulo")
	}
	if got.Name != "Minha Usina" {
		t.Fatalf("name was erased: got %q, want %q", got.Name, "Minha Usina")
	}
}

func TestScheduleSkipsPlantWithoutTimezone(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	pValid, err := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P_VALID", Name: "Valid TZ", Timezone: "America/Sao_Paulo"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpsertDevice(ctx, domain.Device{PlantID: pValid.ID, ExternalID: "INV_V", Name: "Inv V", Kind: "inverter"})
	if err != nil {
		t.Fatal(err)
	}

	pInvalid, err := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P_INVALID", Name: "Invalid TZ", Timezone: ""})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpsertDevice(ctx, domain.Device{PlantID: pInvalid.ID, ExternalID: "INV_I", Name: "Inv I", Kind: "inverter"})
	if err != nil {
		t.Fatal(err)
	}

	conn := growatt.New("https://example.com", "tok", nil)

	if err := ScheduleCompletedDays(ctx, st, src.ID, conn, now); err != nil {
		t.Fatal(err)
	}

	var invalidCount int
	err = st.DB().QueryRowContext(ctx, `SELECT count(*) FROM fetch_tasks WHERE task_key LIKE '%plant_id=P_INVALID%'`).Scan(&invalidCount)
	if err != nil {
		t.Fatal(err)
	}
	if invalidCount != 0 {
		t.Fatalf("expected 0 tasks for plant with empty timezone, got %d", invalidCount)
	}

	err = Backfill(ctx, st, src.ID, conn, pInvalid.ID, "2026-11-01", "2026-11-02")
	if err == nil {
		t.Fatal("expected error from Backfill for plant without timezone, got nil")
	}
}

func TestSchedulerDiscoversOncePerUTCDay(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	conn := growatt.New("https://example.com", "tok", nil)
	logger := log.New(io.Discard, "", 0)

	sched := NewScheduler(st, src.ID, conn, WithSchedulerNow(now), WithSchedulerLogger(logger))

	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	stats, _ := st.GetQueueStats(ctx, src.ID)
	if stats.Pending < 1 {
		t.Fatalf("expected discovery enqueued on tick 1, got %d pending", stats.Pending)
	}

	// Mark discovery task done on Nov 4
	_, _ = st.DB().ExecContext(ctx, `UPDATE fetch_tasks SET status = 'done', updated_at = ? WHERE task_key LIKE 'plant/list%'`, currentTime.Format(time.RFC3339))

	// Tick 2 (15 minutes later on same UTC day): must NOT trigger discovery again
	currentTime = currentTime.Add(15 * time.Minute)
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var plantListPending int
	_ = st.DB().QueryRowContext(ctx, `SELECT count(*) FROM fetch_tasks WHERE task_key LIKE 'plant/list%' AND status = 'pending'`).Scan(&plantListPending)
	if plantListPending != 0 {
		t.Fatal("discovery should not run twice on the same UTC day")
	}

	// Tick 3 (next UTC day: Nov 5): triggers discovery again
	currentTime = time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	_ = st.DB().QueryRowContext(ctx, `SELECT count(*) FROM fetch_tasks WHERE task_key LIKE 'plant/list%' AND status = 'pending'`).Scan(&plantListPending)
	if plantListPending != 1 {
		t.Fatalf("expected discovery to run on new UTC day, got %d pending", plantListPending)
	}
}

func TestRefreshFindsNewDevice(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	devicesList := []connector.DeviceInfo{
		{PlantExternalID: "P1", ExternalID: "INV1", Name: "Inverter 1", Kind: "inverter"},
	}

	conn := growatt.New("https://example.com", "tok", nil)

	// 1. Initial discovery produces plant/list, which produces device/list followup
	discTasks := conn.Discovery()
	if err := st.EnqueueTask(ctx, src.ID, discTasks[0], currentTime, false); err != nil {
		t.Fatal(err)
	}

	mock := &mockConnector{
		execute: func(ctx context.Context, task connector.Task) (connector.Result, error) {
			switch task.Endpoint {
			case "plant/list":
				// Produces plant P1 and followups
				return conn.Execute(ctx, task)
			case "plant/details":
				return connector.Result{
					Plants: []connector.PlantInfo{{ExternalID: "P1", Name: "Plant 1", Timezone: "America/Sao_Paulo"}},
					Raw:    []byte(`{"error_code": 0}`),
				}, nil
			case "device/list":
				return connector.Result{
					Plants:  []connector.PlantInfo{{ExternalID: "P1", Name: "Plant 1", Timezone: "America/Sao_Paulo"}},
					Devices: devicesList,
					Raw:     []byte(`{"error_code": 0}`),
				}, nil
			}
			return connector.Result{Raw: []byte(`{"error_code": 0}`)}, nil
		},
	}

	runner := NewRunner(st, src.ID, mock, WithNow(now))

	// Step 1: plant/list runs and enqueues followups (plant/details, device/list)
	// Mock server for plant/list:
	mock.execute = func(ctx context.Context, task connector.Task) (connector.Result, error) {
		if task.Endpoint == "plant/list" {
			devListParams := map[string]string{"plant_id": "P1", "page": "1", "perpage": "100"}
			return connector.Result{
				Plants: []connector.PlantInfo{{ExternalID: "P1", Name: "Plant 1"}},
				Followups: []connector.Task{
					{
						Endpoint:    "device/list",
						Params:      devListParams,
						LimitKey:    growatt.MakeLimitKey("device/list", devListParams),
						Refreshable: true,
					},
				},
				Raw: []byte(`{"error_code": 0}`),
			}, nil
		}
		if task.Endpoint == "device/list" {
			return connector.Result{
				Plants:  []connector.PlantInfo{{ExternalID: "P1", Name: "Plant 1", Timezone: "America/Sao_Paulo"}},
				Devices: devicesList,
				Raw:     []byte(`{"error_code": 0}`),
			}, nil
		}
		return connector.Result{Raw: []byte(`{"error_code": 0}`)}, nil
	}

	// Run plant/list
	proc, err := runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("plant/list step: proc=%v err=%v", proc, err)
	}

	// Run device/list
	proc, err = runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("device/list step 1: proc=%v err=%v", proc, err)
	}

	devs, err := st.ListDevices(ctx, 1)
	if err != nil || len(devs) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devs))
	}

	// 2. Later, provider added INV2
	devicesList = append(devicesList, connector.DeviceInfo{
		PlantExternalID: "P1", ExternalID: "INV2", Name: "Inverter 2", Kind: "inverter",
	})

	// Re-run plant/list (e.g. daily discovery refresh). Its followup device/list MUST be refreshed!
	currentTime = currentTime.Add(24 * time.Hour)
	if err := st.EnqueueTask(ctx, src.ID, discTasks[0], currentTime, true); err != nil {
		t.Fatal(err)
	}

	// Run plant/list again
	proc, err = runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("plant/list step 2: proc=%v err=%v", proc, err)
	}

	// If followups were enqueued with refresh=false, device/list would be skipped because it is 'done'!
	// With refresh=t.Refreshable, device/list is pending and runs!
	proc, err = runner.Step(ctx)
	if err != nil || !proc {
		t.Fatalf("refreshed device/list should run, but proc=%v err=%v", proc, err)
	}

	devs, err = st.ListDevices(ctx, 1)
	if err != nil || len(devs) != 2 {
		t.Fatalf("expected 2 devices after refresh, got %d", len(devs))
	}
}

func TestStartAllSchedules(t *testing.T) {
	st := newTestStore(t, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	os.Setenv("TOKEN_SRC_SCHED", "valid-secret-token")
	defer os.Unsetenv("TOKEN_SRC_SCHED")

	src, err := st.EnsureSource(ctx, domain.Source{
		Kind:          "growatt",
		Name:          "source_sched",
		CredentialEnv: "TOKEN_SRC_SCHED",
		ConfigJSON:    `{"base_url": "https://example.com"}`,
		Enabled:       true,
	})
	if err != nil {
		t.Fatal(err)
	}

	logger := log.New(io.Discard, "", 0)
	if err := NewManager(ctx, st, nil, logger).StartAll(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	var discoveryCount int
	err = st.DB().QueryRowContext(ctx, `SELECT count(*) FROM fetch_tasks WHERE source_id = ? AND task_key LIKE 'plant/list%'`, src.ID).Scan(&discoveryCount)
	if err != nil {
		t.Fatal(err)
	}
	if discoveryCount < 1 {
		t.Fatal("expected StartAll to run scheduler and enqueue discovery")
	}
}

func TestScheduleCatchesUpAfterDowntime(t *testing.T) {
	currentTime := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	p, err := st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "Plant 1", Timezone: "America/Sao_Paulo"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpsertDevice(ctx, domain.Device{PlantID: p.ID, ExternalID: "INV1", Name: "Inv 1", Kind: "inverter"})
	if err != nil {
		t.Fatal(err)
	}

	conn := growatt.New("https://example.com", "tok", nil)

	if err := ScheduleCompletedDays(ctx, st, src.ID, conn, now); err != nil {
		t.Fatal(err)
	}

	var count int
	err = st.DB().QueryRowContext(ctx, `SELECT count(*) FROM fetch_tasks WHERE source_id = ? AND json_extract(task_json, '$.endpoint') = 'device/inverter/day_energy'`, src.ID).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 7 {
		t.Fatalf("expected 7 completed days enqueued after downtime, got %d", count)
	}
}

func TestNoSyntheticDeviceDuringDiscovery(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	_, err = st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "Plant 1", Timezone: "America/Sao_Paulo"})
	if err != nil {
		t.Fatal(err)
	}

	discoveryTask := connector.Task{
		Endpoint: "device/list",
		Params:   map[string]string{"plant_id": "P1", "page": "1", "perpage": "100"},
		LimitKey: "device/list?page=1&perpage=100&plant_id=P1",
	}
	if err := st.EnqueueTask(ctx, src.ID, discoveryTask, currentTime, false); err != nil {
		t.Fatal(err)
	}

	conn := growatt.New("https://example.com", "tok", nil)

	if err := ScheduleCompletedDays(ctx, st, src.ID, conn, now); err != nil {
		t.Fatal(err)
	}

	var plantEnergyCount int
	err = st.DB().QueryRowContext(ctx, `SELECT count(*) FROM fetch_tasks WHERE source_id = ? AND json_extract(task_json, '$.endpoint') = 'plant/energy'`, src.ID).Scan(&plantEnergyCount)
	if err != nil {
		t.Fatal(err)
	}
	if plantEnergyCount != 0 {
		t.Fatalf("expected 0 synthetic plant/energy tasks during discovery, got %d", plantEnergyCount)
	}
}

func TestStorageErrorCountsAttempt(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx := context.Background()

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	task := connector.Task{
		Endpoint: "plant/details",
		Params:   map[string]string{"plant_id": "1"},
		LimitKey: "plant/details?plant_id=1",
	}
	if err := st.EnqueueTask(ctx, src.ID, task, currentTime, false); err != nil {
		t.Fatal(err)
	}

	mock := &mockConnector{
		execute: func(ctx context.Context, task connector.Task) (connector.Result, error) {
			return connector.Result{
				Plants: []connector.PlantInfo{
					{ExternalID: "P1", Name: "Plant 1", Timezone: "America/Sao_Paulo"},
				},
				Raw: []byte(`{"error_code": 0}`),
			}, nil
		},
	}

	if _, err := st.DB().ExecContext(ctx, `DROP TABLE raw_payloads`); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(st, src.ID, mock, WithNow(now))

	proc, err := runner.Step(ctx)
	if err == nil {
		t.Fatal("expected error due to missing raw_payloads table, got nil")
	}
	if !proc {
		t.Fatal("expected task to be processed")
	}

	var attempts int
	var status string
	_ = st.DB().QueryRowContext(ctx, `SELECT attempts, status FROM fetch_tasks WHERE task_key = 'plant/details?plant_id=1'`).Scan(&attempts, &status)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if status != "pending" {
		t.Fatalf("status = %s, want pending", status)
	}
}

func TestCancelDuringExecuteDoesNotCountAttempt(t *testing.T) {
	currentTime := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }

	st := newTestStore(t, now)
	ctx, cancel := context.WithCancel(context.Background())

	src, err := st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "g1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	task := connector.Task{
		Endpoint: "plant/details",
		Params:   map[string]string{"plant_id": "1"},
		LimitKey: "plant/details?plant_id=1",
	}
	if err := st.EnqueueTask(ctx, src.ID, task, currentTime, false); err != nil {
		t.Fatal(err)
	}

	mock := &mockConnector{
		execute: func(ctx context.Context, task connector.Task) (connector.Result, error) {
			cancel()
			return connector.Result{}, ctx.Err()
		},
	}

	runner := NewRunner(st, src.ID, mock, WithNow(now))

	proc, err := runner.Step(ctx)
	if proc {
		t.Fatal("expected proc=false on cancelled context, got true")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	var attempts int
	_ = st.DB().QueryRowContext(context.Background(), `SELECT attempts FROM fetch_tasks WHERE task_key = 'plant/details?plant_id=1'`).Scan(&attempts)
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0", attempts)
	}
}
