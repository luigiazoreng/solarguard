package growatt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
)

func TestEveryCallIsGET(t *testing.T) {
	methods := make(map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods[r.URL.Path] = r.Method
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "plant/list"):
			w.Write([]byte(`{"error_code": 0, "error_msg": "", "data": {"plants": [{"plant_id": 1, "plant_name": "P1"}], "count": 1}}`))
		case strings.Contains(r.URL.Path, "plant/details"):
			w.Write([]byte(`{"error_code": 0, "error_msg": "", "data": {"timezone": "-3", "latitude": -23.5, "longitude": -46.6, "peak_power": 50.0}}`))
		case strings.Contains(r.URL.Path, "device/list"):
			w.Write([]byte(`{"error_code": 0, "error_msg": "", "data": {"devices": [{"device_sn": "INV1", "device_name": "Inverter 1", "lost": 0}], "count": 1}}`))
		case strings.Contains(r.URL.Path, "device/inverter/day_energy"):
			w.Write([]byte(`{"error_code": 0, "error_msg": "", "device_sn": "INV1", "dataloggerSn": "DL1", "data": "12.5"}`))
		case strings.Contains(r.URL.Path, "plant/energy"):
			w.Write([]byte(`{"error_code": 0, "error_msg": "", "data": {"energys": [{"date": "2026-11-04", "energy": "34.2"}]}}`))
		default:
			w.Write([]byte(`{"error_code": 0, "error_msg": "", "data": {}}`))
		}
	}))
	defer server.Close()

	c := New(server.URL, "secret-token-123", server.Client())
	ctx := context.Background()

	tasks := []connector.Task{
		{Endpoint: "plant/list", Params: map[string]string{"page": "1", "perpage": "100"}},
		{Endpoint: "plant/details", Params: map[string]string{"plant_id": "1"}},
		{Endpoint: "device/list", Params: map[string]string{"plant_id": "1", "page": "1", "perpage": "100"}},
		{Endpoint: "device/inverter/day_energy", Params: map[string]string{"device_sn": "INV1", "date": "2026-11-04", "plant_id": "1", "timezone": "America/Sao_Paulo"}},
		{Endpoint: "plant/energy", Params: map[string]string{"plant_id": "1", "start_date": "2026-11-04", "end_date": "2026-11-04", "time_unit": "day", "timezone": "America/Sao_Paulo"}},
	}

	for _, task := range tasks {
		res, err := c.Execute(ctx, task)
		if err != nil {
			t.Fatalf("task %s failed: %v", task.Endpoint, err)
		}
		if len(res.Raw) == 0 {
			t.Errorf("task %s returned empty Raw", task.Endpoint)
		}
	}

	if len(methods) != len(tasks) {
		t.Fatalf("expected %d requests, got %d", len(tasks), len(methods))
	}
	for path, method := range methods {
		if method != http.MethodGet {
			t.Errorf("path %s called with %s, want GET", path, method)
		}
	}
}

func TestTokenOnlyInHeader(t *testing.T) {
	token := "super-secret-token-999"
	var receivedToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedToken = r.Header.Get("token")
		if strings.Contains(r.URL.RawQuery, token) {
			t.Errorf("token leaked in query string: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		// Simulate an error message containing the token to test that connector sanitizes it
		w.Write([]byte(fmt.Sprintf(`{"error_code": 9999, "error_msg": "token %s invalid", "data": "failed"}`, token)))
	}))
	defer server.Close()

	c := New(server.URL, token, server.Client())
	ctx := context.Background()

	// 1. Check Discovery tasks do not carry token
	for _, task := range c.Discovery() {
		taskJSON, _ := json.Marshal(task)
		if strings.Contains(string(taskJSON), token) {
			t.Fatal("token found in discovery task")
		}
	}

	// 2. Check DailyTasks do not carry token
	p := domain.Plant{ExternalID: "P1", Timezone: "America/Sao_Paulo"}
	d := []domain.Device{{ExternalID: "INV1"}}
	for _, task := range c.DailyTasks(p, d, "2026-11-04") {
		taskJSON, _ := json.Marshal(task)
		if strings.Contains(string(taskJSON), token) {
			t.Fatal("token found in daily tasks")
		}
	}

	// 3. Execute call and verify error does not contain token
	_, err := c.Execute(ctx, connector.Task{
		Endpoint: "plant/list",
		Params:   map[string]string{"page": "1", "perpage": "100"},
	})
	if err == nil {
		t.Fatal("expected error from server")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in error message: %v", err)
	}

	// 4. Verify token was sent in the header
	if receivedToken != token {
		t.Fatalf("got token header %q, want %q", receivedToken, token)
	}
}

func TestLimitKeyStability(t *testing.T) {
	params1 := map[string]string{
		"date":      "2026-11-04",
		"device_sn": "INV123",
		"plant_id":  "42",
		"timezone":  "America/Sao_Paulo",
	}
	params2 := map[string]string{
		"timezone":  "America/Sao_Paulo",
		"plant_id":  "42",
		"date":      "2026-11-04",
		"device_sn": "INV123",
	}

	k1 := MakeLimitKey("device/inverter/day_energy", params1)
	k2 := MakeLimitKey("device/inverter/day_energy", params2)

	want := "device/inverter/day_energy?date=2026-11-04&device_sn=INV123&plant_id=42&timezone=America/Sao_Paulo"
	if k1 != want {
		t.Fatalf("got %q, want %q", k1, want)
	}
	if k1 != k2 {
		t.Fatalf("limit key not stable across map order: %q != %q", k1, k2)
	}

	// Empty params
	kEmpty := MakeLimitKey("plant/list", nil)
	if kEmpty != "plant/list?" {
		t.Fatalf("got %q, want plant/list?", kEmpty)
	}
}

func TestRateLimit10012(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error_code": 10012, "error_msg": "frequently access", "data": "error"}`))
	}))
	defer server.Close()

	c := New(server.URL, "tok", server.Client())
	_, err := c.Execute(context.Background(), connector.Task{
		Endpoint: "plant/list",
		Params:   map[string]string{"page": "1", "perpage": "100"},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
	var rle *connector.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitError, got %T", err)
	}
	if rle.RetryAfter != 5*time.Minute {
		t.Fatalf("got RetryAfter %v, want 5m", rle.RetryAfter)
	}
}

func TestNonZeroErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"error_code": 10005, "error_msg": "invalid param", "data": "failed"}`))
	}))
	defer server.Close()

	c := New(server.URL, "tok", server.Client())
	_, err := c.Execute(context.Background(), connector.Task{
		Endpoint: "plant/details",
		Params:   map[string]string{"plant_id": "1"},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "10005") || !strings.Contains(err.Error(), "invalid param") {
		t.Fatalf("expected error code and message in error, got: %v", err)
	}
}

func TestDiscovery(t *testing.T) {
	c := New("https://example.com", "tok", nil)
	tasks := c.Discovery()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 discovery task, got %d", len(tasks))
	}
	dt := tasks[0]
	if dt.Endpoint != "plant/list" {
		t.Errorf("endpoint = %s, want plant/list", dt.Endpoint)
	}
	if dt.Params["page"] != "1" || dt.Params["perpage"] != "100" {
		t.Errorf("params = %v", dt.Params)
	}
	if dt.DailyBudgetKey != "plant/list" || dt.DailyBudget != 10 {
		t.Errorf("daily budget key = %s, budget = %d", dt.DailyBudgetKey, dt.DailyBudget)
	}
	if dt.MinInterval != 5*time.Minute {
		t.Errorf("min_interval = %v, want 5m", dt.MinInterval)
	}
}

func TestExecutePlantListWithPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// count is 150, page 1 perpage 100 -> should generate followup for page 2
		w.Write([]byte(`{
			"error_code": 0,
			"error_msg": "",
			"data": {
				"plants": [
					{"plant_id": 101, "plant_name": "Fazenda Sol"}
				],
				"count": 150
			}
		}`))
	}))
	defer server.Close()

	c := New(server.URL, "tok", server.Client())
	res, err := c.Execute(context.Background(), connector.Task{
		Endpoint: "plant/list",
		Params:   map[string]string{"page": "1", "perpage": "100"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plants) != 1 || res.Plants[0].ExternalID != "101" || res.Plants[0].Name != "Fazenda Sol" {
		t.Fatalf("unexpected plants: %+v", res.Plants)
	}

	// Should have followups:
	// 1. plant/list page 2
	// 2. plant/details for plant 101
	// 3. device/list for plant 101
	if len(res.Followups) != 3 {
		t.Fatalf("expected 3 followups, got %d: %+v", len(res.Followups), res.Followups)
	}
	var hasNextPage, hasDetails, hasDevList bool
	for _, f := range res.Followups {
		switch f.Endpoint {
		case "plant/list":
			if f.Params["page"] == "2" {
				hasNextPage = true
			}
		case "plant/details":
			if f.Params["plant_id"] == "101" {
				hasDetails = true
			}
		case "device/list":
			if f.Params["plant_id"] == "101" && f.Params["page"] == "1" {
				hasDevList = true
			}
		}
	}
	if !hasNextPage || !hasDetails || !hasDevList {
		t.Fatalf("missing followup: next_page=%v details=%v devlist=%v", hasNextPage, hasDetails, hasDevList)
	}
}

func TestExecutePlantDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// peak_power as string or number
		w.Write([]byte(`{
			"error_code": 0,
			"error_msg": "",
			"data": {
				"plant_id": 202,
				"plant_name": "Parque Solar",
				"timezone": "-3",
				"latitude": "-15.78",
				"longitude": -47.92,
				"peak_power": "75.5"
			}
		}`))
	}))
	defer server.Close()

	c := New(server.URL, "tok", server.Client())
	res, err := c.Execute(context.Background(), connector.Task{
		Endpoint: "plant/details",
		Params:   map[string]string{"plant_id": "202"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plants) != 1 {
		t.Fatalf("expected 1 plant, got %d", len(res.Plants))
	}
	p := res.Plants[0]
	if p.ExternalID != "202" {
		t.Errorf("plant external id = %s, want 202", p.ExternalID)
	}
	// Timezone must be a valid zone name Go can load
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		t.Fatalf("timezone %q is not loadable by time.LoadLocation: %v", p.Timezone, err)
	}
	if p.Latitude == nil || *p.Latitude != -15.78 {
		t.Errorf("latitude = %v, want -15.78", p.Latitude)
	}
	if p.Longitude == nil || *p.Longitude != -47.92 {
		t.Errorf("longitude = %v, want -47.92", p.Longitude)
	}
	if p.PeakPowerKW == nil || *p.PeakPowerKW != 75.5 {
		t.Errorf("peak_power = %v, want 75.5", p.PeakPowerKW)
	}
}

func TestExecuteDeviceList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"error_code": 0,
			"error_msg": "",
			"data": {
				"devices": [
					{"device_sn": "INV-A", "device_name": "Inversor A", "lost": 0},
					{"device_sn": "INV-B", "device_name": "Inversor B", "lost": 1}
				],
				"count": 2
			}
		}`))
	}))
	defer server.Close()

	c := New(server.URL, "tok", server.Client())
	res, err := c.Execute(context.Background(), connector.Task{
		Endpoint: "device/list",
		Params:   map[string]string{"plant_id": "100", "page": "1", "perpage": "100"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(res.Devices))
	}
	dA := res.Devices[0]
	if dA.ExternalID != "INV-A" || dA.Online == nil || *dA.Online != true {
		t.Errorf("dA online = %v, want true", dA.Online)
	}
	dB := res.Devices[1]
	if dB.ExternalID != "INV-B" || dB.Online == nil || *dB.Online != false {
		t.Errorf("dB online = %v, want false", dB.Online)
	}
}

func TestExecuteDayEnergy_BothShapes(t *testing.T) {
	// Shape 1: Root level fields (documented in GROWATT_API.md)
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"error_code": 0,
			"error_msg": "",
			"device_sn": "INV001",
			"dataloggerSn": "DL001",
			"data": "123.45"
		}`))
	}))
	defer s1.Close()

	c1 := New(s1.URL, "tok", s1.Client())
	task := connector.Task{
		Endpoint: "device/inverter/day_energy",
		Params: map[string]string{
			"device_sn": "INV001",
			"date":      "2026-11-04",
			"plant_id":  "P10",
			"timezone":  "America/Sao_Paulo",
		},
	}
	res1, err := c1.Execute(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if len(res1.Readings) != 1 {
		t.Fatalf("expected 1 reading, got %d", len(res1.Readings))
	}
	r1 := res1.Readings[0]
	if r1.PlantExternalID != "P10" || r1.DeviceExternalID != "INV001" {
		t.Errorf("IDs = %s, %s", r1.PlantExternalID, r1.DeviceExternalID)
	}
	if r1.RawValue != "123.45" {
		t.Errorf("raw_value = %q, want 123.45", r1.RawValue)
	}
	if r1.Value == nil || *r1.Value != 123.45 {
		t.Errorf("value = %v, want 123.45", r1.Value)
	}
	if r1.Metric != domain.MetricEnergyKWh {
		t.Errorf("metric = %s, want %s", r1.Metric, domain.MetricEnergyKWh)
	}
	wantStart, wantEnd, _ := domain.LocalDay("2026-11-04", "America/Sao_Paulo")
	if !r1.PeriodStart.Equal(wantStart) || !r1.PeriodEnd.Equal(wantEnd) {
		t.Errorf("period = %s to %s, want %s to %s", r1.PeriodStart, r1.PeriodEnd, wantStart, wantEnd)
	}

	// Shape 2: Nested under data (standard envelope)
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"error_code": 0,
			"error_msg": "",
			"data": {
				"device_sn": "INV001",
				"dataloggerSn": "DL001",
				"data": "98.7"
			}
		}`))
	}))
	defer s2.Close()

	c2 := New(s2.URL, "tok", s2.Client())
	res2, err := c2.Execute(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Readings) != 1 {
		t.Fatalf("expected 1 reading, got %d", len(res2.Readings))
	}
	r2 := res2.Readings[0]
	if r2.RawValue != "98.7" || r2.Value == nil || *r2.Value != 98.7 {
		t.Errorf("r2 value = %v, raw = %q", r2.Value, r2.RawValue)
	}
}

func TestDailyTasksWithDevices(t *testing.T) {
	c := New("https://example.com", "tok", nil)
	plant := domain.Plant{ExternalID: "P1", Timezone: "America/Sao_Paulo"}
	devices := []domain.Device{
		{ExternalID: "INV1", Kind: "inverter"},
		{ExternalID: "INV2", Kind: "inverter"},
	}

	tasks := c.DailyTasks(plant, devices, "2026-11-04")
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	for i, task := range tasks {
		if task.Endpoint != "device/inverter/day_energy" {
			t.Errorf("endpoint = %s, want device/inverter/day_energy", task.Endpoint)
		}
		if task.Params["device_sn"] != devices[i].ExternalID {
			t.Errorf("device_sn = %s, want %s", task.Params["device_sn"], devices[i].ExternalID)
		}
		if task.Params["plant_id"] != "P1" || task.Params["date"] != "2026-11-04" || task.Params["timezone"] != "America/Sao_Paulo" {
			t.Errorf("params mismatch: %v", task.Params)
		}
		if task.MinInterval != 5*time.Minute {
			t.Errorf("min_interval = %v, want 5m", task.MinInterval)
		}
	}
}

func TestDailyTasksWithoutDevices(t *testing.T) {
	c := New("https://example.com", "tok", nil)
	plant := domain.Plant{ExternalID: "P99", Timezone: "America/Sao_Paulo"}
	var devices []domain.Device

	tasks := c.DailyTasks(plant, devices, "2026-11-04")
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task for plant without devices, got %d", len(tasks))
	}
	task := tasks[0]
	if task.Endpoint != "plant/energy" {
		t.Errorf("endpoint = %s, want plant/energy", task.Endpoint)
	}
	if task.DailyBudgetKey != "plant/energy" || task.DailyBudget != 10 {
		t.Errorf("daily budget key = %s, budget = %d", task.DailyBudgetKey, task.DailyBudget)
	}
	if task.Params["plant_id"] != "P99" || task.Params["start_date"] != "2026-11-04" || task.Params["end_date"] != "2026-11-04" || task.Params["time_unit"] != "day" {
		t.Errorf("params mismatch: %v", task.Params)
	}
}

func TestExecutePlantEnergy_SyntheticDevice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"error_code": 0,
			"error_msg": "",
			"data": {
				"energys": [
					{"date": "2026-11-04", "energy": "154.2"}
				]
			}
		}`))
	}))
	defer server.Close()

	c := New(server.URL, "tok", server.Client())
	task := connector.Task{
		Endpoint: "plant/energy",
		Params: map[string]string{
			"plant_id":   "P99",
			"start_date": "2026-11-04",
			"end_date":   "2026-11-04",
			"time_unit":  "day",
			"timezone":   "America/Sao_Paulo",
		},
	}
	res, err := c.Execute(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}

	// Must produce synthetic device
	if len(res.Devices) != 1 {
		t.Fatalf("expected 1 synthetic device, got %d", len(res.Devices))
	}
	dev := res.Devices[0]
	if dev.ExternalID != "plant:P99" || dev.Kind != "plant" || dev.PlantExternalID != "P99" {
		t.Fatalf("unexpected synthetic device: %+v", dev)
	}

	// Reading goes to synthetic device
	if len(res.Readings) != 1 {
		t.Fatalf("expected 1 reading, got %d", len(res.Readings))
	}
	r := res.Readings[0]
	if r.DeviceExternalID != "plant:P99" || r.RawValue != "154.2" || *r.Value != 154.2 {
		t.Fatalf("unexpected reading: %+v", r)
	}
}

func TestParseTimezone(t *testing.T) {
	cases := []struct {
		input     string
		defaultTZ string
		wantLoad  string
	}{
		{"America/Sao_Paulo", "UTC", "America/Sao_Paulo"},
		{"-3", "America/Sao_Paulo", "America/Sao_Paulo"},
		{"+8", "America/Sao_Paulo", "Etc/GMT-8"},
		{"-03:00", "America/Sao_Paulo", "America/Sao_Paulo"},
		{"", "America/Sao_Paulo", "America/Sao_Paulo"},
		{"invalid/zone/name", "America/Sao_Paulo", "America/Sao_Paulo"},
	}

	for _, tc := range cases {
		got := ParseTimezone(tc.input, tc.defaultTZ)
		if got == "" {
			t.Errorf("ParseTimezone(%q, %q) returned empty string", tc.input, tc.defaultTZ)
		}
		if _, err := time.LoadLocation(got); err != nil {
			t.Errorf("ParseTimezone(%q, %q) = %q, not loadable by time.LoadLocation: %v", tc.input, tc.defaultTZ, got, err)
		}
		if tc.wantLoad != "" && got != tc.wantLoad {
			t.Errorf("ParseTimezone(%q, %q) = %q, want %q", tc.input, tc.defaultTZ, got, tc.wantLoad)
		}
	}
}
