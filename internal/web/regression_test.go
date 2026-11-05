package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/web"
)

// startServer serves the panel as if an account that owns every source were logged in.
func startServer(t *testing.T, st *store.Store, now time.Time) *httptest.Server {
	t.Helper()
	srv, err := web.NewServer(web.Config{
		Store: st,
		Now:   func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := ownerServer(t, st, srv)
	t.Cleanup(ts.Close)
	return ts
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", url, resp.StatusCode, body)
	}
	return string(body)
}

func dayReading(t *testing.T, st *store.Store, plant domain.Plant, deviceID int64, date string, value float64, q domain.Quality) {
	t.Helper()
	start, end, err := domain.LocalDay(date, plant.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertReadings(context.Background(), []domain.Reading{{
		DeviceID: deviceID, Metric: domain.MetricEnergyKWh,
		PeriodStart: start, PeriodEnd: end, Value: &value, Quality: q,
	}}); err != nil {
		t.Fatal(err)
	}
}

func dayExpected(t *testing.T, st *store.Store, plantID, deviceID int64, date string, kwh float64) {
	t.Helper()
	ctx := context.Background()
	run, err := st.GetActiveModelRun(ctx, plantID)
	if err != nil {
		run, err = st.UpsertModelRun(ctx, store.ModelRun{Name: "calibrated", Version: "1", PlantID: plantID, TrainedAt: time.Now(), Active: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetActiveModelRun(ctx, plantID, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertPredictions(ctx, []store.Prediction{{DeviceID: deviceID, Date: date, ModelRunID: run.ID, KWhExpected: kwh}}); err != nil {
		t.Fatal(err)
	}
}

// Every dashboard write depends on the CSRF check seeing the session, so this
// goes through what a browser does: open a page, send back the token it rendered.
func TestPOST_WithTokenFromRenderedPage(t *testing.T) {
	st := newWebTestStore(t)
	plant, d1, _ := seedPlantWithDevices(t, st, "P1", "Planta 1")
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)

	saveAlert := func(key string) store.Alert {
		a := store.Alert{PlantID: plant.ID, DeviceID: &d1.ID, Rule: "under_expected", Severity: "warning", Status: "open",
			DedupeKey: key, Title: "Abaixo", FirstDate: "2026-05-01", LastDate: "2026-05-01"}
		if err := st.SaveAlert(context.Background(), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	post := func(c *http.Client, url string, form url.Values, origin string) int {
		req, _ := http.NewRequest("POST", url, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	ts := startServer(t, st, now)
	client, token := sessionClient(t, ts.URL)
	alert := saveAlert("ack")
	target := fmt.Sprintf("%s/alerts/%d/ack", ts.URL, alert.ID)

	if code := post(client, target, url.Values{"csrf_token": {"wrong"}}, ts.URL); code != http.StatusForbidden {
		t.Errorf("wrong token: status %d, want 403", code)
	}
	if code := post(client, target, url.Values{"csrf_token": {token}, "note": {"visto"}}, ts.URL); code != http.StatusSeeOther {
		t.Errorf("rendered token: status %d, want 303", code)
	}
	got, err := st.GetAlert(context.Background(), alert.ID)
	if err != nil || got.Status != "ack" {
		t.Errorf("alert status %q (err %v), want ack", got.Status, err)
	}
}

// Hand-computed: only plant C has both values yesterday, 110 against 100, so +10,0%.
// Summing what exists on each side would give (160 - 300) / 300 = -46,7%.
func TestOverview_DeviationPairsOnlyPlantsWithBothValues(t *testing.T) {
	st := newWebTestStore(t)
	now := time.Date(2026, 5, 2, 15, 0, 0, 0, time.UTC)
	yesterday := "2026-05-01"

	pA, a1, a2 := seedPlantWithDevices(t, st, "A", "Usina A")
	dayReading(t, st, pA, a1.ID, yesterday, 50, domain.QualityOK)
	_ = a2

	pB, b1, _ := seedPlantWithDevices(t, st, "B", "Usina B")
	dayExpected(t, st, pB.ID, b1.ID, yesterday, 200)
	dayReading(t, st, pB, b1.ID, "2026-04-20", 30, domain.QualityOK)
	dayReading(t, st, pB, b1.ID, yesterday, 0, domain.QualityInvalid) // dead inverter reporting zero

	pC, c1, _ := seedPlantWithDevices(t, st, "C", "Usina C")
	dayReading(t, st, pC, c1.ID, yesterday, 110, domain.QualityOK)
	dayExpected(t, st, pC.ID, c1.ID, yesterday, 100)

	body := getBody(t, startServer(t, st, now).URL+"/")

	if !strings.Contains(body, "+10,0%") {
		t.Errorf("expected the paired deviation +10,0%%")
	}
	if strings.Contains(body, "46,7") {
		t.Errorf("deviation mixes plants that have only one of the two values")
	}
	if !strings.Contains(body, "20/04/2026") {
		t.Errorf("plant B should show its last valid reading, 20/04/2026")
	}
	if !strings.Contains(body, "2 de 3 usinas com leitura válida") {
		t.Errorf("expected the count of plants with a valid reading")
	}
}

func TestAlertDetail_OfflineCountsElapsedDays(t *testing.T) {
	cases := []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"under one day", 23 * time.Hour, "Sem comunicação há menos de 1 dia"},
		{"one day", 25 * time.Hour, "Sem comunicação há 1 dia"},
		{"two days", 49 * time.Hour, "Sem comunicação há 2 dias"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newWebTestStore(t)
			ctx := context.Background()
			now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
			plant, d1, _ := seedPlantWithDevices(t, st, "P", "Usina")
			lastSeen := now.Add(-tc.ago)
			offline := false
			if _, err := st.UpsertDevice(ctx, domain.Device{ID: d1.ID, PlantID: plant.ID, ExternalID: d1.ExternalID,
				Name: d1.Name, Kind: d1.Kind, Online: &offline, LastSeenAt: &lastSeen}); err != nil {
				t.Fatal(err)
			}
			alert := store.Alert{PlantID: plant.ID, DeviceID: &d1.ID, Rule: "device_offline", Severity: "critical", Status: "open",
				DedupeKey: "off", Title: "Sem comunicação", FirstDate: "2026-05-08", LastDate: "2026-05-10",
				EvidenceJSON: `{"num_days":3,"first_date":"2026-05-08"}`}
			if err := st.SaveAlert(ctx, &alert); err != nil {
				t.Fatal(err)
			}
			body := getBody(t, fmt.Sprintf("%s/alerts/%d", startServer(t, st, now).URL, alert.ID))
			if !strings.Contains(body, tc.want) {
				t.Errorf("expected %q", tc.want)
			}
		})
	}
}

func TestPlant_DeviceFilterAppliesToLostEnergy(t *testing.T) {
	st := newWebTestStore(t)
	now := time.Date(2026, 5, 3, 15, 0, 0, 0, time.UTC)
	plant, d1, d2 := seedPlantWithDevices(t, st, "P", "Usina")
	date := "2026-05-02"
	dayReading(t, st, plant, d1.ID, date, 25, domain.QualityOK)
	dayExpected(t, st, plant.ID, d1.ID, date, 26)
	dayExpected(t, st, plant.ID, d2.ID, date, 37.5) // d2 has no reading

	ts := startServer(t, st, now)
	base := fmt.Sprintf("%s/plants/%d?period=7", ts.URL, plant.ID)
	if body := getBody(t, base); !strings.Contains(body, "37,5") {
		t.Errorf("whole plant should show 37,5 kWh expected without measurement")
	}
	if body := getBody(t, fmt.Sprintf("%s&device=%d", base, d1.ID)); strings.Contains(body, "37,5") {
		t.Errorf("filter on d1 must not count the missing day of d2")
	}
	if body := getBody(t, base+"&device=999999"); !strings.Contains(body, "37,5") {
		t.Errorf("an unknown device should fall back to the whole plant")
	}
}

func TestPlant_PeriodWithoutReadingsIsNotShownAsZero(t *testing.T) {
	st := newWebTestStore(t)
	now := time.Date(2026, 5, 30, 15, 0, 0, 0, time.UTC)
	plant, d1, _ := seedPlantWithDevices(t, st, "P", "Usina")
	dayReading(t, st, plant, d1.ID, "2026-04-01", 25, domain.QualityOK)

	ts := startServer(t, st, now)
	body := getBody(t, fmt.Sprintf("%s/plants/%d?period=7", ts.URL, plant.ID))
	if !strings.Contains(body, "sem leituras no período") {
		t.Errorf("expected the empty period message")
	}
	if !strings.Contains(body, "Dados até 01/04/2026") {
		t.Errorf("expected the date of the newest reading")
	}
	if strings.Contains(body, "25,0") {
		t.Errorf("the reading of a day outside the period must not be counted")
	}
}

func TestSeries_RejectsRangeAboveLimit(t *testing.T) {
	st := newWebTestStore(t)
	plant, _, _ := seedPlantWithDevices(t, st, "P", "Usina")
	ts := startServer(t, st, time.Date(2026, 5, 3, 15, 0, 0, 0, time.UTC))
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/plants/%d/series?from=0001-01-01&to=2026-05-01", ts.URL, plant.ID))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

func TestPlant_ModelErrorAndDeviceStatusAreShownCorrectly(t *testing.T) {
	st := newWebTestStore(t)
	ctx := context.Background()
	plant, d1, _ := seedPlantWithDevices(t, st, "P", "Usina")
	run, err := st.UpsertModelRun(ctx, store.ModelRun{Name: "ridge", Version: "1", PlantID: plant.ID, TrainedAt: time.Now(), Active: true,
		MetricsJSON: `{"walk_forward":{"pooled":{"mae":4.45,"rmse":6.1,"r2":0.85,"wape":0.137},"devices":{}}}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetActiveModelRun(ctx, plant.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	dayReading(t, st, plant, d1.ID, "2026-05-01", 25, domain.QualityOK)

	body := getBody(t, fmt.Sprintf("%s/plants/%d", startServer(t, st, time.Date(2026, 5, 3, 15, 0, 0, 0, time.UTC)).URL, plant.ID))
	if !strings.Contains(body, "13,7%") {
		t.Errorf("WAPE is stored as a fraction and must show as 13,7%%")
	}
	if strings.Contains(body, "Online") || !strings.Contains(body, "Não informado") {
		t.Errorf("a device with no reported status, such as an imported one, must not show as Online")
	}
}
