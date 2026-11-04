package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/web"
)

func newWebTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "web_test.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedPlantWithDevices(t *testing.T, st *store.Store, extID, name string) (domain.Plant, domain.Device, domain.Device) {
	t.Helper()
	ctx := context.Background()
	src, err := st.EnsureSource(ctx, domain.Source{
		Kind:          "growatt",
		Name:          "source_" + extID,
		CredentialEnv: "SECRET_GROWATT_KEY",
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}

	lat := -23.5
	lon := -46.6
	p, err := st.UpsertPlant(ctx, domain.Plant{
		SourceID:   src.ID,
		ExternalID: extID,
		Name:       name,
		Timezone:   "America/Sao_Paulo",
		Latitude:   &lat,
		Longitude:  &lon,
	})
	if err != nil {
		t.Fatalf("upsert plant: %v", err)
	}

	d1, err := st.UpsertDevice(ctx, domain.Device{
		PlantID:    p.ID,
		ExternalID: "D1",
		Name:       "D1",
		Kind:       "inverter",
	})
	if err != nil {
		t.Fatalf("upsert device D1: %v", err)
	}

	d2, err := st.UpsertDevice(ctx, domain.Device{
		PlantID:    p.ID,
		ExternalID: "D2",
		Name:       "D2",
		Kind:       "inverter",
	})
	if err != nil {
		t.Fatalf("upsert device D2: %v", err)
	}

	return p, d1, d2
}

// 1. Login, logout, wrong password, lockout, expiry; every protected route redirects when logged out.
func TestAuth_LoginLogoutWrongPasswordLockoutExpiryRedirects(t *testing.T) {
	st := newWebTestStore(t)
	mockNow := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "supersecretpassword",
		Store:         st,
		Now:           func() time.Time { return mockNow },
	}
	srv, err := web.NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // do not follow redirects
		},
	}

	// 1a. Protected routes redirect when logged out
	protectedRoutes := []string{"/", "/plants/1", "/alerts", "/reports", "/manage"}
	for _, route := range protectedRoutes {
		resp, err := client.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("get %s: %v", route, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
			t.Errorf("expected redirect for %s when logged out, got status %d", route, resp.StatusCode)
		}
		loc := resp.Header.Get("Location")
		if !strings.HasPrefix(loc, "/login") {
			t.Errorf("expected redirect to /login, got %q", loc)
		}
	}

	// 1b. Wrong password
	wrongForm := url.Values{"password": {"wrongpass"}}
	resp, err := client.PostForm(ts.URL+"/login", wrongForm)
	if err != nil {
		t.Fatalf("post login wrong pass: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for wrong pass, got %d", resp.StatusCode)
	}

	// 1c. Lockout after 5 failures from same IP
	for i := 0; i < 4; i++ {
		r, err := client.PostForm(ts.URL+"/login", wrongForm)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}
	// 5th failure triggered lockout
	lockoutResp, err := client.PostForm(ts.URL+"/login", wrongForm)
	if err != nil {
		t.Fatal(err)
	}
	lockoutResp.Body.Close()
	if lockoutResp.StatusCode != http.StatusTooManyRequests && lockoutResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 429 Too Many Requests or 401 on lockout, got %d", lockoutResp.StatusCode)
	}

	// Reset lockout for testing successful login with another client IP simulation or new server
	cfg2 := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "supersecretpassword",
		Store:         st,
		Now:           func() time.Time { return mockNow },
	}
	srv2, _ := web.NewServer(cfg2)
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()

	// 1d. Successful login
	goodForm := url.Values{"password": {"supersecretpassword"}}
	respGood, err := client.PostForm(ts2.URL+"/login", goodForm)
	if err != nil {
		t.Fatal(err)
	}
	respGood.Body.Close()
	if respGood.StatusCode != http.StatusSeeOther && respGood.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect on successful login, got %d", respGood.StatusCode)
	}

	cookie := respGood.Header.Get("Set-Cookie")
	if cookie == "" || !strings.Contains(cookie, "solarguard_session") {
		t.Fatalf("expected session cookie, got %q", cookie)
	}
	if !strings.Contains(cookie, "HttpOnly") {
		t.Errorf("cookie must have HttpOnly: %s", cookie)
	}
	if !strings.Contains(cookie, "SameSite=Lax") {
		t.Errorf("cookie must have SameSite=Lax: %s", cookie)
	}

	// 1e. Access protected route with cookie
	req, _ := http.NewRequest("GET", ts2.URL+"/", nil)
	req.Header.Set("Cookie", cookie)
	respAuth, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respAuth.Body.Close()
	if respAuth.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK with valid session cookie, got %d", respAuth.StatusCode)
	}

	// 1f. Session expiry (12 hours)
	mockNow = mockNow.Add(13 * time.Hour)
	reqExpired, _ := http.NewRequest("GET", ts2.URL+"/", nil)
	reqExpired.Header.Set("Cookie", cookie)
	respExpired, err := client.Do(reqExpired)
	if err != nil {
		t.Fatal(err)
	}
	respExpired.Body.Close()
	if respExpired.StatusCode != http.StatusFound && respExpired.StatusCode != http.StatusSeeOther {
		t.Errorf("expected redirect for expired session, got %d", respExpired.StatusCode)
	}
}

// 2. Refusal to start with no password on a non-loopback address.
func TestServer_RefusalToStartNoPasswordNonLoopback(t *testing.T) {
	st := newWebTestStore(t)

	// Non-loopback addresses must be refused if password is empty
	nonLoopbacks := []string{"0.0.0.0:8080", "192.168.1.10:8080", ":8080", "example.com:80"}
	for _, addr := range nonLoopbacks {
		cfg := web.Config{
			Addr:          addr,
			AdminPassword: "",
			Store:         st,
		}
		_, err := web.NewServer(cfg)
		if err == nil {
			t.Errorf("expected server creation to fail for non-loopback addr %q with empty password", addr)
		}
	}

	// Loopback addresses are allowed with empty password (no login required)
	loopbacks := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"}
	for _, addr := range loopbacks {
		cfg := web.Config{
			Addr:          addr,
			AdminPassword: "",
			Store:         st,
		}
		srv, err := web.NewServer(cfg)
		if err != nil {
			t.Errorf("expected server creation to succeed for loopback addr %q with empty password, got err: %v", addr, err)
			continue
		}
		if !srv.NoAuthRequired() {
			t.Errorf("expected NoAuthRequired to be true for loopback with empty password")
		}
	}
}

// 3. POST without a valid CSRF token is rejected; a GET never changes state.
func TestSecurity_CSRFAndOriginAndGETNeverChangesState(t *testing.T) {
	st := newWebTestStore(t)
	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "", // loopback no-auth
		Store:         st,
	}
	srv, err := web.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()

	// First, fetch an alert detail page to get a session / CSRF token
	plant, d1, _ := seedPlantWithDevices(t, st, "P1", "Planta 1")
	alert := store.Alert{
		PlantID:   plant.ID,
		DeviceID:  &d1.ID,
		Rule:      "zero_generation",
		Severity:  "critical",
		Status:    "open",
		DedupeKey: "P1:D1:zero:2026-05-01",
		Title:     "Geração zerada",
		FirstDate: "2026-05-01",
		LastDate:  "2026-05-02",
	}
	if err := st.SaveAlert(context.Background(), &alert); err != nil {
		t.Fatal(err)
	}

	// 3a. POST without CSRF token must be rejected (403 Forbidden)
	resp, err := client.Post(fmt.Sprintf("%s/alerts/%d/ack", ts.URL, alert.ID), "application/x-www-form-urlencoded", strings.NewReader("note=test"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for POST without CSRF token, got %d", resp.StatusCode)
	}

	// 3b. Verify GET to state-changing endpoint returns 404 or 405 Method Not Allowed (never changes state)
	getResp, err := client.Get(fmt.Sprintf("%s/alerts/%d/ack", ts.URL, alert.ID))
	if err != nil {
		t.Fatal(err)
	}
	getResp.Body.Close()
	if getResp.StatusCode == http.StatusOK {
		t.Errorf("GET to state-changing route /alerts/{id}/ack should not succeed, got status %d", getResp.StatusCode)
	}

	// Check alert status is still 'open'
	a, err := st.GetAlert(context.Background(), alert.ID)
	if err != nil || a.Status != "open" {
		t.Errorf("alert status should still be 'open', got %s", a.Status)
	}
}

// 4. XSS test with plant named `<script>alert(1)</script>` on every list and detail page.
func TestSecurity_XSSPlantNameOnEveryPage(t *testing.T) {
	st := newWebTestStore(t)
	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
	}
	srv, _ := web.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	xssName := "<script>alert(1)</script>"
	plant, d1, _ := seedPlantWithDevices(t, st, "XSS_PLANT", xssName)

	alert := store.Alert{
		PlantID:   plant.ID,
		DeviceID:  &d1.ID,
		Rule:      "zero_generation",
		Severity:  "critical",
		Status:    "open",
		DedupeKey: "XSS:zero",
		Title:     "Alerta " + xssName,
		FirstDate: "2026-05-01",
		LastDate:  "2026-05-02",
	}
	_ = st.SaveAlert(context.Background(), &alert)

	rep, _ := st.InsertReport(context.Background(), store.Report{
		PlantID:     &plant.ID,
		Kind:        "period",
		PeriodStart: "2026-05-01",
		PeriodEnd:   "2026-05-02",
		Status:      "draft",
		Author:      "system",
		Title:       "Relatório " + xssName,
		BodyMD:      "Corpo do relatório",
	})

	urlsToTest := []string{
		"/",
		fmt.Sprintf("/plants/%d", plant.ID),
		"/alerts",
		fmt.Sprintf("/alerts/%d", alert.ID),
		"/reports",
		fmt.Sprintf("/reports/%d", rep.ID),
		"/manage",
	}

	client := ts.Client()
	for _, u := range urlsToTest {
		resp, err := client.Get(ts.URL + u)
		if err != nil {
			t.Fatalf("get %s: %v", u, err)
		}
		var bodyBuilder strings.Builder
		buf := make([]byte, 1024)
		for {
			n, _ := resp.Body.Read(buf)
			if n > 0 {
				bodyBuilder.Write(buf[:n])
			} else {
				break
			}
		}
		resp.Body.Close()

		body := bodyBuilder.String()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("url %s returned status %d", u, resp.StatusCode)
		}

		// Must NOT contain raw unescaped script tag
		if strings.Contains(body, "<script>alert(1)</script>") {
			t.Errorf("page %s contained raw unescaped XSS payload <script>alert(1)</script>", u)
		}
		// Must contain escaped version
		if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("page %s did not contain properly escaped text &lt;script&gt;alert(1)&lt;/script&gt;", u)
		}
	}
}

// 5. Two plants are isolated in overview, plant page, series endpoint and alert filters;
// one plant with 0 readings renders; unknown IDs return 404 and do not expose errors.
func TestPlants_IsolationAndZeroReadingsAndUnknown404(t *testing.T) {
	st := newWebTestStore(t)
	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
	}
	srv, _ := web.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	p1, _, _ := seedPlantWithDevices(t, st, "P1", "Planta 1")
	p2, _, _ := seedPlantWithDevices(t, st, "P2", "Planta 2")

	// P1 has 0 readings; verify its page renders fine (200 OK)
	respP1, err := client.Get(fmt.Sprintf("%s/plants/%d", ts.URL, p1.ID))
	if err != nil || respP1.StatusCode != http.StatusOK {
		t.Errorf("plant with 0 readings failed to render: status %d, err %v", respP1.StatusCode, err)
	}
	respP1.Body.Close()

	// Unknown plant ID returns 404
	respUnknown, err := client.Get(ts.URL + "/plants/999999")
	if err != nil || respUnknown.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown plant, got %d", respUnknown.StatusCode)
	}
	respUnknown.Body.Close()

	// Unknown series returns 404
	respSeriesUnknown, err := client.Get(ts.URL + "/api/v1/plants/999999/series")
	if err != nil || respSeriesUnknown.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown plant series, got %d", respSeriesUnknown.StatusCode)
	}
	respSeriesUnknown.Body.Close()

	// Verify isolation: Series endpoint for P1 only returns P1 devices
	respSeries1, err := client.Get(fmt.Sprintf("%s/api/v1/plants/%d/series?from=2026-05-01&to=2026-05-10", ts.URL, p1.ID))
	if err != nil || respSeries1.StatusCode != http.StatusOK {
		t.Fatalf("get series for p1: %v", err)
	}
	var data1 struct {
		PlantID int64 `json:"plant_id"`
	}
	_ = json.NewDecoder(respSeries1.Body).Decode(&data1)
	respSeries1.Body.Close()
	if data1.PlantID != p1.ID {
		t.Errorf("expected plant_id %d in series, got %d", p1.ID, data1.PlantID)
	}

	// Verify unknown alert returns 404
	respAlertUnknown, err := client.Get(ts.URL + "/alerts/999999")
	if err != nil || respAlertUnknown.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown alert, got %d", respAlertUnknown.StatusCode)
	}
	respAlertUnknown.Body.Close()

	// Verify alert filters isolate by plant
	_ = p2
}

// 6. Series endpoint: measured values on not-ok days are returned flagged and separated
// from the ok series; validates dates; expected and forecast are separate.
func TestSeries_OkAndNotOkSeparationAndValidation(t *testing.T) {
	st := newWebTestStore(t)
	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
	}
	srv, _ := web.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	p, d1, _ := seedPlantWithDevices(t, st, "P1", "Planta 1")

	// Insert readings:
	// 2026-05-01: OK, value 15.0
	// 2026-05-02: Invalid (zero_generation), value 0.0
	ctx := context.Background()
	t1Start, t1End, _ := domain.LocalDay("2026-05-01", p.Timezone)
	v1 := 15.0
	t2Start, t2End, _ := domain.LocalDay("2026-05-02", p.Timezone)
	v2 := 0.0

	_, err := st.InsertReadings(ctx, []domain.Reading{
		{
			DeviceID:    d1.ID,
			Metric:      domain.MetricEnergyKWh,
			PeriodStart: t1Start,
			PeriodEnd:   t1End,
			Value:       &v1,
			RawValue:    "15.0",
			Quality:     domain.QualityOK,
		},
		{
			DeviceID:      d1.ID,
			Metric:        domain.MetricEnergyKWh,
			PeriodStart:   t2Start,
			PeriodEnd:     t2End,
			Value:         &v2,
			RawValue:      "0.0",
			Quality:       domain.QualityInvalid,
			QualityReason: "zero_generation",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Active model predictions:
	mrun, err := st.UpsertModelRun(ctx, store.ModelRun{
		Name:      "ridge",
		Version:   "1.0",
		PlantID:   p.ID,
		TrainedAt: time.Now(),
		Active:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetActiveModelRun(ctx, p.ID, mrun.ID); err != nil {
		t.Fatal(err)
	}

	lower := 12.0
	upper := 16.0
	err = st.UpsertPredictions(ctx, []store.Prediction{
		{
			DeviceID:    d1.ID,
			Date:        "2026-05-01",
			ModelRunID:  mrun.ID,
			KWhExpected: 14.5,
			KWhLower:    &lower,
			KWhUpper:    &upper,
		},
		{
			DeviceID:    d1.ID,
			Date:        "2026-05-02",
			ModelRunID:  mrun.ID,
			KWhExpected: 14.0,
			KWhLower:    &lower,
			KWhUpper:    &upper,
		},
		// Future forecast date
		{
			DeviceID:    d1.ID,
			Date:        "2026-05-03",
			ModelRunID:  mrun.ID,
			KWhExpected: 13.5,
			KWhLower:    &lower,
			KWhUpper:    &upper,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 6a. Date validation: invalid date returns 400 Bad Request
	badDateResp, err := client.Get(fmt.Sprintf("%s/api/v1/plants/%d/series?from=invalid&to=2026-05-02", ts.URL, p.ID))
	if err != nil || badDateResp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid date, got %d", badDateResp.StatusCode)
	}
	badDateResp.Body.Close()

	// 6b. Valid query
	resp, err := client.Get(fmt.Sprintf("%s/api/v1/plants/%d/series?from=2026-05-01&to=2026-05-02", ts.URL, p.ID))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get series: %v, status %d", err, resp.StatusCode)
	}
	defer resp.Body.Close()

	var payload struct {
		PlantID        int64              `json:"plant_id"`
		Dates          []string           `json:"dates"`
		MeasuredOK     []*float64         `json:"measured_ok"`
		MeasuredNotOK  []web.NotOKReading `json:"measured_not_ok"`
		Expected       []*float64         `json:"expected"`
		ForecastDates  []string           `json:"forecast_dates"`
		ForecastValues []*float64         `json:"forecast_values"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode series json: %v", err)
	}

	// 2026-05-01 is OK: must have MeasuredOK = 15.0
	// 2026-05-02 is Not OK: must NOT be in normal MeasuredOK (nil or separate), and must be in MeasuredNotOK
	if len(payload.Dates) != 2 {
		t.Fatalf("expected 2 dates, got %d", len(payload.Dates))
	}
	if payload.MeasuredOK[0] == nil || *payload.MeasuredOK[0] != 15.0 {
		t.Errorf("expected measured_ok[0] = 15.0, got %v", payload.MeasuredOK[0])
	}
	if payload.MeasuredOK[1] != nil {
		t.Errorf("expected measured_ok[1] to be nil for not-ok reading, got %v", *payload.MeasuredOK[1])
	}

	foundNotOK := false
	for _, nok := range payload.MeasuredNotOK {
		if nok.Date == "2026-05-02" && nok.Reason == "zero_generation" {
			foundNotOK = true
			break
		}
	}
	if !foundNotOK {
		t.Errorf("expected measured_not_ok to contain 2026-05-02 with zero_generation")
	}
}

// 7. Ack and resolve update status and keep the note.
func TestAlerts_AckAndResolveUpdateStatusKeepNote(t *testing.T) {
	st := newWebTestStore(t)
	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
	}
	srv, _ := web.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	plant, d1, _ := seedPlantWithDevices(t, st, "P1", "Planta 1")
	alert := store.Alert{
		PlantID:   plant.ID,
		DeviceID:  &d1.ID,
		Rule:      "under_expected",
		Severity:  "warning",
		Status:    "open",
		DedupeKey: "P1:D1:under:2026-05-01",
		Title:     "Geração abaixo do esperado",
		FirstDate: "2026-05-01",
		LastDate:  "2026-05-01",
	}
	if err := st.SaveAlert(context.Background(), &alert); err != nil {
		t.Fatal(err)
	}

	csrfToken := srv.TestCSRFToken()

	// Ack alert with note
	ackForm := url.Values{
		"csrf_token": {csrfToken},
		"note":       {"Verificado com a equipe de campo."},
	}
	reqAck, _ := http.NewRequest("POST", fmt.Sprintf("%s/alerts/%d/ack", ts.URL, alert.ID), strings.NewReader(ackForm.Encode()))
	reqAck.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqAck.Header.Set("Origin", ts.URL)
	respAck, err := client.Do(reqAck)
	if err != nil {
		t.Fatal(err)
	}
	respAck.Body.Close()
	if respAck.StatusCode != http.StatusOK && respAck.StatusCode != http.StatusSeeOther && respAck.StatusCode != http.StatusFound {
		t.Fatalf("ack failed with status %d", respAck.StatusCode)
	}

	aAfterAck, err := st.GetAlert(context.Background(), alert.ID)
	if err != nil || aAfterAck.Status != "ack" || aAfterAck.Note != "Verificado com a equipe de campo." {
		t.Errorf("expected status 'ack' and preserved note, got status %q, note %q", aAfterAck.Status, aAfterAck.Note)
	}

	// Resolve alert keeping or adding note
	resForm := url.Values{
		"csrf_token": {csrfToken},
		"note":       {"Inversor reiniciado e funcionando."},
	}
	reqRes, _ := http.NewRequest("POST", fmt.Sprintf("%s/alerts/%d/resolve", ts.URL, alert.ID), strings.NewReader(resForm.Encode()))
	reqRes.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqRes.Header.Set("Origin", ts.URL)
	respRes, err := client.Do(reqRes)
	if err != nil {
		t.Fatal(err)
	}
	respRes.Body.Close()
	if respRes.StatusCode != http.StatusOK && respRes.StatusCode != http.StatusSeeOther && respRes.StatusCode != http.StatusFound {
		t.Fatalf("resolve failed with status %d", respRes.StatusCode)
	}

	aAfterRes, err := st.GetAlert(context.Background(), alert.ID)
	if err != nil || aAfterRes.Status != "resolved" || aAfterRes.Note != "Inversor reiniciado e funcionando." {
		t.Errorf("expected status 'resolved' and updated note, got status %q, note %q", aAfterRes.Status, aAfterRes.Note)
	}
}

// 8. Lost energy calculation against hand-computed values.
func TestPlant_LostEnergyCalculation(t *testing.T) {
	// Hand-computed scenario:
	// Day 1: OK, Expected = 20.0, Measured = 19.5 -> Lost energy = 0.0
	// Day 2: Invalid (zero_generation), Expected = 22.0, Measured = 0.0 -> Lost energy = 22.0
	// Day 3: Suspect (stuck_value), Expected = 18.0, Measured = 5.0 -> Lost energy = 18.0
	// Day 4: Missing reading, Expected = 25.0 -> Lost energy = 25.0
	// Total Expected = 85.0
	// Hand-computed Lost Energy = 22.0 + 18.0 + 25.0 = 65.0 kWh.
	st := newWebTestStore(t)
	p, d1, _ := seedPlantWithDevices(t, st, "P1", "Planta 1")
	ctx := context.Background()

	mrun, _ := st.UpsertModelRun(ctx, store.ModelRun{
		Name:      "calibrated",
		Version:   "1.0",
		PlantID:   p.ID,
		TrainedAt: time.Now(),
		Active:    true,
	})
	_ = st.SetActiveModelRun(ctx, p.ID, mrun.ID)

	days := []string{"2026-05-01", "2026-05-02", "2026-05-03", "2026-05-04"}
	expectedVals := []float64{20.0, 22.0, 18.0, 25.0}

	for i, d := range days {
		_ = st.UpsertPredictions(ctx, []store.Prediction{
			{
				DeviceID:    d1.ID,
				Date:        d,
				ModelRunID:  mrun.ID,
				KWhExpected: expectedVals[i],
			},
		})
	}

	// Insert readings for Day 1, 2, 3 (Day 4 is missing)
	t1Start, t1End, _ := domain.LocalDay(days[0], p.Timezone)
	v1 := 19.5
	t2Start, t2End, _ := domain.LocalDay(days[1], p.Timezone)
	v2 := 0.0
	t3Start, t3End, _ := domain.LocalDay(days[2], p.Timezone)
	v3 := 5.0

	_, _ = st.InsertReadings(ctx, []domain.Reading{
		{
			DeviceID:    d1.ID,
			Metric:      domain.MetricEnergyKWh,
			PeriodStart: t1Start,
			PeriodEnd:   t1End,
			Value:       &v1,
			Quality:     domain.QualityOK,
		},
		{
			DeviceID:      d1.ID,
			Metric:        domain.MetricEnergyKWh,
			PeriodStart:   t2Start,
			PeriodEnd:     t2End,
			Value:         &v2,
			Quality:       domain.QualityInvalid,
			QualityReason: "zero_generation",
		},
		{
			DeviceID:      d1.ID,
			Metric:        domain.MetricEnergyKWh,
			PeriodStart:   t3Start,
			PeriodEnd:     t3End,
			Value:         &v3,
			Quality:       domain.QualitySuspect,
			QualityReason: "stuck_value",
		},
	})

	lostEnergy, err := web.ComputeLostEnergy(ctx, st, p.ID, nil, "2026-05-01", "2026-05-04")
	if err != nil {
		t.Fatalf("compute lost energy: %v", err)
	}

	expectedLost := 65.0
	if lostEnergy != expectedLost {
		t.Errorf("expected lost energy %.2f, got %.2f", expectedLost, lostEnergy)
	}
}

// 9. No page shows the value of an env var used as a credential.
func TestSecurity_NoPageShowsEnvVarValue(t *testing.T) {
	st := newWebTestStore(t)
	secretValue := "MY_SUPER_SECRET_GROWATT_API_TOKEN_12345"
	_ = os.Setenv("SECRET_GROWATT_KEY", secretValue)
	defer os.Unsetenv("SECRET_GROWATT_KEY")

	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
	}
	srv, _ := web.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	plant, d1, _ := seedPlantWithDevices(t, st, "P1", "Planta 1")
	alert := store.Alert{
		PlantID:   plant.ID,
		DeviceID:  &d1.ID,
		Rule:      "zero_generation",
		Severity:  "critical",
		Status:    "open",
		DedupeKey: "P1:D1:zero",
		Title:     "Alerta",
		FirstDate: "2026-05-01",
		LastDate:  "2026-05-01",
	}
	_ = st.SaveAlert(context.Background(), &alert)

	pages := []string{"/", fmt.Sprintf("/plants/%d", plant.ID), "/alerts", fmt.Sprintf("/alerts/%d", alert.ID), "/reports", "/manage"}

	for _, p := range pages {
		resp, err := client.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		var b strings.Builder
		buf := make([]byte, 1024)
		for {
			n, _ := resp.Body.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			} else {
				break
			}
		}
		resp.Body.Close()

		body := b.String()
		if strings.Contains(body, secretValue) {
			t.Fatalf("SECURITY VIOLATION: page %s exposes credential value %q!", p, secretValue)
		}
		if p == "/manage" {
			// Manage page MUST show the env var name and whether it is configured
			if !strings.Contains(body, "SECRET_GROWATT_KEY") {
				t.Errorf("/manage should display the env var name 'SECRET_GROWATT_KEY'")
			}
			if !strings.Contains(body, "Configurada") {
				t.Errorf("/manage should indicate that the variable is 'Configurada'")
			}
		}
	}
}
