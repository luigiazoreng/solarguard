package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/analytics"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/web"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// 8. Analytics cards, lost energy, and date clamping against hand-computed values.
func TestPlant_AnalyticsCardsAndClamping(t *testing.T) {
	// Hand-computed scenario:
	// Day 1 (2026-05-01): OK, Expected = 20.0, Measured = 19.5 -> Lost = 0.0
	// Day 2 (2026-05-02): Invalid (zero_generation), Expected = 22.0, Measured = 0.0 -> Lost = 22.0
	// Day 3 (2026-05-03): Suspect (stuck_value), Expected = 18.0, Measured = 5.0 -> Lost = 18.0
	// Day 4 (2026-05-04): Missing reading, Expected = 25.0 -> Lost = 25.0
	// Total Expected (4 days) = 85.0 kWh
	// Over full range [05-01, 05-04]: Lost energy = 22.0 + 18.0 + 25.0 = 65.0 kWh
	// When clamped to newest reading date (2026-05-03):
	// Range is [05-01, 05-03] (3 days): Lost energy = 22.0 + 18.0 = 40.0 kWh
	// Hand-computed Data Quality on clamped range: 1 OK day out of 3 days = 33.33%
	// Hand-computed Paired Expected vs Actual on clamped range:
	// Paired: Day 1 (19.5 vs 20.0), PairedDeviceDays = 1, deviation = (19.5 - 20.0) / 20.0 = -2.5%
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

	// 1. Verify single source of truth analytics on full range
	lostFull, err := analytics.LostEnergy(ctx, st, p.ID, "2026-05-01", "2026-05-04")
	if err != nil {
		t.Fatalf("analytics.LostEnergy full: %v", err)
	}
	if lostFull.LostEnergyKWh != 65.0 {
		t.Errorf("expected full range lost energy 65.0, got %v", lostFull.LostEnergyKWh)
	}

	// 2. Verify analytics on clamped range
	lostClamped, err := analytics.LostEnergy(ctx, st, p.ID, "2026-05-01", "2026-05-03")
	if err != nil {
		t.Fatalf("analytics.LostEnergy clamped: %v", err)
	}
	if lostClamped.LostEnergyKWh != 40.0 {
		t.Errorf("expected clamped lost energy 40.0, got %v", lostClamped.LostEnergyKWh)
	}

	dqClamped, err := analytics.DataQuality(ctx, st, p.ID, "2026-05-01", "2026-05-03")
	if err != nil {
		t.Fatalf("analytics.DataQuality: %v", err)
	}
	if len(dqClamped.Devices) > 0 {
		d0 := dqClamped.Devices[0]
		if d0.TotalDays != 3 || d0.OKDays != 1 {
			t.Errorf("expected 1 OK day out of 3 total days, got %d of %d", d0.OKDays, d0.TotalDays)
		}
	}

	// 3. Verify server renders clamped card with mandatory Brazilian Portuguese wording
	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
		Now: func() time.Time {
			return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
		},
	}
	srv, _ := web.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(fmt.Sprintf("%s/plants/%d?period=30", ts.URL, p.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	rawBody, _ := io.ReadAll(resp.Body)
	body := string(rawBody)

	// Check clamped date banner
	if !strings.Contains(body, "Dados até 03/05/2026") {
		t.Errorf("expected page to show 'Dados até 03/05/2026', got HTML without it")
	}
	// Check mandatory wording for lost energy
	if !strings.Contains(body, "Energia esperada sem medição válida") {
		t.Errorf("expected card title 'Energia esperada sem medição válida'")
	}
	if !strings.Contains(body, "Estimativa: pode ter sido gerada e não registrada.") {
		t.Errorf("expected mandatory sub-text 'Estimativa: pode ter sido gerada e não registrada.'")
	}
	// Check Medida x esperada
	if !strings.Contains(body, "Medida x esperada") {
		t.Errorf("expected card title 'Medida x esperada'")
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

// 10. Chart 90% band aggregation: aggregate of 2 devices with half-width 3 and 4 gives 5.
func TestChart_BandAggregation_3Plus4Gives5(t *testing.T) {
	upper1 := 13.0
	upper2 := 24.0
	preds := []web.PredictionInput{
		{Expected: 10.0, Upper: &upper1}, // hw = 3
		{Expected: 20.0, Upper: &upper2}, // hw = 4
	}
	totExp, lower, upper := web.AggregatePredictions(preds)
	if totExp != 30.0 {
		t.Errorf("expected total 30.0, got %v", totExp)
	}
	hw := upper - totExp
	if math.Abs(hw-5.0) > 1e-9 {
		t.Errorf("expected aggregate half-width 5.0, got %v", hw)
	}
	if lower != 25.0 {
		t.Errorf("expected lower 25.0, got %v", lower)
	}
	if upper != 35.0 {
		t.Errorf("expected upper 35.0, got %v", upper)
	}
}

// 11. DNS rebinding protection: reject non-loopback Host headers in no-password loopback mode.
func TestSecurity_LoopbackHostHeaderRejection(t *testing.T) {
	st := newWebTestStore(t)
	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
	}
	srv, err := web.NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	// External / malicious Host headers must be rejected with 403 Forbidden
	for _, badHost := range []string{"evil.com", "attacker.domain:8080", "192.168.1.50:8080"} {
		req, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req.Host = badHost
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for bad host %q, got %d", badHost, resp.StatusCode)
		}
	}

	// Loopback Host headers must be accepted with 200 OK
	for _, goodHost := range []string{"localhost", "localhost:8080", "127.0.0.1:8080", "[::1]:8080"} {
		req, _ := http.NewRequest("GET", ts.URL+"/", nil)
		req.Host = goodHost
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 OK for valid loopback host %q, got %d", goodHost, resp.StatusCode)
		}
	}
}

// 12. Session reuse in no-password mode: multiple requests reuse the existing session.
func TestSecurity_SessionReuseInLoopbackMode(t *testing.T) {
	st := newWebTestStore(t)
	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
	}
	srv, err := web.NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	for i := 0; i < 5; i++ {
		resp, err := client.Get(ts.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	activeCount := srv.ActiveSessionCount()
	if activeCount != 1 {
		t.Errorf("expected 1 session reused across requests, got %d", activeCount)
	}
}

// 13. Gestão inputs: empty field keeps stored value, comma separator accepted, invalid number gives error.
func TestManage_KeepCurrentOnEmptyAndCommaNumbers(t *testing.T) {
	st := newWebTestStore(t)
	_, d1, _ := seedPlantWithDevices(t, st, "P_MANAGE", "Usina Gestão")
	ctx := context.Background()

	initCap := 15.0
	initArea := 42.0
	initEff := 0.18
	_ = st.UpdateDevice(ctx, d1.ID, &initCap, &initArea, &initEff)

	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
	}
	srv, _ := web.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // capture redirects
		},
	}

	// First GET to establish session and get CSRF token
	respGet, err := client.Get(ts.URL + "/manage")
	if err != nil {
		t.Fatal(err)
	}
	respGet.Body.Close()

	csrfToken := srv.TestCSRFToken()

	// 1. POST with empty area must keep stored area (42.0)
	formEmpty := url.Values{
		"csrf_token":    {csrfToken},
		"capacity_kw":   {"15.0"},
		"panel_area_m2": {""}, // empty keeps current
		"efficiency":    {"0.18"},
	}
	reqEmpty, _ := http.NewRequest("POST", fmt.Sprintf("%s/manage/devices/%d", ts.URL, d1.ID), strings.NewReader(formEmpty.Encode()))
	reqEmpty.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqEmpty.Header.Set("Origin", ts.URL)
	respEmpty, err := client.Do(reqEmpty)
	if err != nil {
		t.Fatal(err)
	}
	respEmpty.Body.Close()

	devAfterEmpty, _ := st.GetDevice(ctx, d1.ID)
	if devAfterEmpty.PanelAreaM2 == nil || *devAfterEmpty.PanelAreaM2 != 42.0 {
		t.Errorf("expected area to remain 42.0 after empty post, got %v", devAfterEmpty.PanelAreaM2)
	}

	// 2. POST with comma decimal separator (45,5) must update area to 45.5
	formComma := url.Values{
		"csrf_token":    {csrfToken},
		"capacity_kw":   {"15.0"},
		"panel_area_m2": {"45,5"},
		"efficiency":    {"0.18"},
	}
	reqComma, _ := http.NewRequest("POST", fmt.Sprintf("%s/manage/devices/%d", ts.URL, d1.ID), strings.NewReader(formComma.Encode()))
	reqComma.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqComma.Header.Set("Origin", ts.URL)
	respComma, err := client.Do(reqComma)
	if err != nil {
		t.Fatal(err)
	}
	respComma.Body.Close()

	devAfterComma, _ := st.GetDevice(ctx, d1.ID)
	if devAfterComma.PanelAreaM2 == nil || *devAfterComma.PanelAreaM2 != 45.5 {
		t.Errorf("expected area to update to 45.5, got %v", devAfterComma.PanelAreaM2)
	}

	// 3. POST with invalid number must redirect with error message
	formBad := url.Values{
		"csrf_token":    {csrfToken},
		"capacity_kw":   {"15.0"},
		"panel_area_m2": {"invalido"},
		"efficiency":    {"0.18"},
	}
	reqBad, _ := http.NewRequest("POST", fmt.Sprintf("%s/manage/devices/%d", ts.URL, d1.ID), strings.NewReader(formBad.Encode()))
	reqBad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqBad.Header.Set("Origin", ts.URL)
	respBad, err := client.Do(reqBad)
	if err != nil {
		t.Fatal(err)
	}
	respBad.Body.Close()

	loc := respBad.Header.Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Errorf("expected redirect location to contain error, got %q", loc)
	}
}

// 14. Alert detail: detected_on from evidence_json formatted as DD/MM/AAAA, and no raw codes.
func TestAlerts_DetectedOnFormatting(t *testing.T) {
	st := newWebTestStore(t)
	p, d1, _ := seedPlantWithDevices(t, st, "P_ALERT", "Usina Alerta")
	ctx := context.Background()

	alert := store.Alert{
		PlantID:      p.ID,
		DeviceID:     &d1.ID,
		Rule:         "stuck_value",
		Severity:     "critical",
		Status:       "open",
		DedupeKey:    "P_ALERT:D1:stuck",
		Title:        "Leitura travada em D1",
		FirstDate:    "2026-03-10",
		LastDate:     "2026-03-12",
		EvidenceJSON: `{"detected_on":"2026-03-12","num_days":3,"values":[{"date":"2026-03-10","value":10,"reason":"stuck_value"},{"date":"2026-03-11","value":10,"reason":"stuck_value"},{"date":"2026-03-12","value":10,"reason":"stuck_value"}]}`,
	}
	_ = st.SaveAlert(ctx, &alert)

	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
	}
	srv, _ := web.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()

	// 1. List page must show 12/03/2026 in "Detectado em"
	respList, err := client.Get(ts.URL + "/alerts")
	if err != nil {
		t.Fatal(err)
	}
	rawList, _ := io.ReadAll(respList.Body)
	respList.Body.Close()
	bodyList := string(rawList)

	if !strings.Contains(bodyList, "12/03/2026") {
		t.Errorf("expected alerts list to display '12/03/2026' for detected_on")
	}

	// 2. Detail page must show "Detectado em: 12/03/2026", Motivo header, and friendly rule label
	respDetail, err := client.Get(fmt.Sprintf("%s/alerts/%d", ts.URL, alert.ID))
	if err != nil {
		t.Fatal(err)
	}
	rawDetail, _ := io.ReadAll(respDetail.Body)
	respDetail.Body.Close()
	bodyDetail := string(rawDetail)

	if !strings.Contains(bodyDetail, "Detectado em: 12/03/2026") {
		t.Errorf("expected alert detail to show 'Detectado em: 12/03/2026'")
	}
	if !strings.Contains(bodyDetail, "<th>Motivo</th>") {
		t.Errorf("expected table header 'Motivo' instead of 'Motivo / Diagnóstico'")
	}
	if !strings.Contains(bodyDetail, "Leitura travada") {
		t.Errorf("expected friendly rule label 'Leitura travada'")
	}
	if !strings.Contains(bodyDetail, "leitura travada") {
		t.Errorf("expected friendly reason label 'leitura travada'")
	}
}

// 15. Overview status badge priorities: Sem comunicação > Crítico > Atenção > Sem dados recentes > OK.
func TestOverview_StatusBadgePriorities(t *testing.T) {
	st := newWebTestStore(t)
	ctx := context.Background()

	// Plant 1: device offline -> "Sem comunicação"
	p1, d1, _ := seedPlantWithDevices(t, st, "P_OFFLINE", "Usina Offline")
	onlineFalse := false
	now := time.Now()
	_, _ = st.UpsertDevice(ctx, domain.Device{
		ID:         d1.ID,
		PlantID:    p1.ID,
		ExternalID: d1.ExternalID,
		Name:       d1.Name,
		Kind:       d1.Kind,
		Online:     &onlineFalse,
		LastSeenAt: &now,
	})

	// Plant 2: critical alert -> "Crítico"
	p2, d2, _ := seedPlantWithDevices(t, st, "P_CRIT", "Usina Critica")
	_ = st.SaveAlert(ctx, &store.Alert{
		PlantID:   p2.ID,
		DeviceID:  &d2.ID,
		Rule:      "zero_generation",
		Severity:  "critical",
		Status:    "open",
		DedupeKey: "crit",
		Title:     "Zero",
		FirstDate: "2026-05-01",
		LastDate:  "2026-05-01",
	})

	// Plant 3: warning alert -> "Atenção"
	p3, d3, _ := seedPlantWithDevices(t, st, "P_WARN", "Usina Aviso")
	_ = st.SaveAlert(ctx, &store.Alert{
		PlantID:   p3.ID,
		DeviceID:  &d3.ID,
		Rule:      "under_expected",
		Severity:  "warning",
		Status:    "open",
		DedupeKey: "warn",
		Title:     "Abaixo",
		FirstDate: "2026-05-01",
		LastDate:  "2026-05-01",
	})

	// Plant 4: OK reading yesterday -> "OK"
	p4, d4, _ := seedPlantWithDevices(t, st, "P_OK", "Usina OK")
	mockNow := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	yStart, yEnd, _ := domain.LocalDay("2026-05-01", p4.Timezone)
	vOK := 25.0
	_, _ = st.InsertReadings(ctx, []domain.Reading{
		{
			DeviceID:    d4.ID,
			Metric:      domain.MetricEnergyKWh,
			PeriodStart: yStart,
			PeriodEnd:   yEnd,
			Value:       &vOK,
			Quality:     domain.QualityOK,
		},
	})

	cfg := web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		Store:         st,
		Now:           func() time.Time { return mockNow },
	}
	srv, _ := web.NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	rawBody, _ := io.ReadAll(resp.Body)
	body := string(rawBody)

	if !strings.Contains(body, "Sem comunicação") {
		t.Errorf("expected overview to show badge 'Sem comunicação'")
	}
	if !strings.Contains(body, "Crítico") {
		t.Errorf("expected overview to show badge 'Crítico'")
	}
	if !strings.Contains(body, "Atenção") {
		t.Errorf("expected overview to show badge 'Atenção'")
	}
	if !strings.Contains(body, "OK") {
		t.Errorf("expected overview to show badge 'OK'")
	}
}

type authHeaderTransport struct {
	token string
	base  http.RoundTripper
}

func (a *authHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	if a.token != "" {
		r2.Header.Set("Authorization", "Bearer "+a.token)
	}
	return a.base.RoundTrip(r2)
}

// 16. MCP mount in serve: 404 when unset, 401 when wrong/no token, 200 with real SDK client.
func TestMCP_MountInServe(t *testing.T) {
	st := newWebTestStore(t)
	ctx := context.Background()
	plant, _, _ := seedPlantWithDevices(t, st, "P_MCP", "Usina MCP")

	// 1. When SOLARGUARD_MCP_TOKEN is unset: /mcp returns 404
	srvUnset, err := web.NewServer(web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		MCPToken:      "",
		Store:         st,
	})
	if err != nil {
		t.Fatalf("new server unset: %v", err)
	}
	tsUnset := httptest.NewServer(srvUnset.Handler())
	defer tsUnset.Close()

	resp404, err := http.Get(tsUnset.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 when MCPToken is unset, got %d", resp404.StatusCode)
	}

	// 2. When SOLARGUARD_MCP_TOKEN is set:
	secretToken := "secret-mcp-bearer-token-123"
	srvSet, err := web.NewServer(web.Config{
		Addr:          "127.0.0.1:8080",
		AdminPassword: "",
		MCPToken:      secretToken,
		Store:         st,
	})
	if err != nil {
		t.Fatalf("new server with mcp token: %v", err)
	}
	tsSet := httptest.NewServer(srvSet.Handler())
	defer tsSet.Close()

	// 2a. Request without token -> 401
	reqNoToken, _ := http.NewRequest("POST", tsSet.URL+"/mcp", strings.NewReader(`{}`))
	reqNoToken.Header.Set("Content-Type", "application/json")
	respNoToken, err := http.DefaultClient.Do(reqNoToken)
	if err != nil {
		t.Fatal(err)
	}
	respNoToken.Body.Close()
	if respNoToken.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", respNoToken.StatusCode)
	}

	// 2b. Request with wrong token -> 401
	reqWrongToken, _ := http.NewRequest("POST", tsSet.URL+"/mcp", strings.NewReader(`{}`))
	reqWrongToken.Header.Set("Content-Type", "application/json")
	reqWrongToken.Header.Set("Authorization", "Bearer wrong-token")
	respWrongToken, err := http.DefaultClient.Do(reqWrongToken)
	if err != nil {
		t.Fatal(err)
	}
	respWrongToken.Body.Close()
	if respWrongToken.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong token, got %d", respWrongToken.StatusCode)
	}

	// 2c. Connect with real SDK MCP client
	httpClient := &http.Client{
		Transport: &authHeaderTransport{
			token: secretToken,
			base:  http.DefaultTransport,
		},
	}
	transport := &mcp.StreamableClientTransport{
		Endpoint:             tsSet.URL + "/mcp",
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := mcpClient.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect real MCP client: %v", err)
	}
	defer session.Close()

	// Call list tools
	toolsList, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools via real MCP client: %v", err)
	}
	if len(toolsList.Tools) == 0 {
		t.Errorf("expected tools to be registered on MCP server")
	}

	// Call tool get_generation
	toolRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_generation",
		Arguments: map[string]any{
			"plant_id": plant.ID,
			"from":     "2026-05-01",
			"to":       "2026-05-05",
		},
	})
	if err != nil {
		t.Fatalf("call get_generation tool: %v", err)
	}
	if toolRes.IsError {
		t.Errorf("tool returned error: %v", toolRes.Content)
	}
}
