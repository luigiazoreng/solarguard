package web_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/ingest"
	"github.com/luigiazoreng/solarguard/internal/secrets"
	"github.com/luigiazoreng/solarguard/internal/store"
	"github.com/luigiazoreng/solarguard/internal/web"
)

const testEmail = "dono@example.com"

func createUser(t *testing.T, st *store.Store, email, password string) store.User {
	t.Helper()
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	user, err := st.CreateUser(ctx, email, string(hash))
	if errors.Is(err, store.ErrEmailTaken) {
		user, err = st.GetUserByEmail(ctx, email)
	}
	if err != nil {
		t.Fatal(err)
	}
	return user
}

// ownerServer serves the panel as an account that owns every source and is
// already logged in through the real login route. The panel's code is untouched:
// the wrapper only adds the cookie a browser would send.
func ownerServer(t *testing.T, st *store.Store, srv *web.Server) *httptest.Server {
	t.Helper()
	user := createUser(t, st, testEmail, "senha-do-dono")

	form := url.Values{"email": {testEmail}, "password": {"senha-do-dono"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusSeeOther || len(cookies) == 0 {
		t.Fatalf("login of the test owner: status %d", rec.Code)
	}
	session := cookies[0]

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fixtures create sources at any moment, always without an owner.
		if _, err := st.DB().ExecContext(r.Context(), `UPDATE sources SET user_id = ? WHERE user_id IS NULL`, user.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := r.Cookie(session.Name); err != nil {
			r.AddCookie(session)
		}
		srv.Handler().ServeHTTP(w, r)
	}))
}

// loginServer serves the real panel, with one account already created.
func loginServer(t *testing.T, st *store.Store, password string) *httptest.Server {
	t.Helper()
	createUser(t, st, testEmail, password)
	return rawServer(t, web.Config{Store: st})
}

func rawServer(t *testing.T, cfg web.Config) *httptest.Server {
	t.Helper()
	srv, err := web.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postForm(t *testing.T, c *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	resp, err := c.PostForm(target, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func getPage(t *testing.T, c *http.Client, target string) (int, string) {
	t.Helper()
	resp, err := c.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func loggedInClient(t *testing.T, baseURL, email, password string) *http.Client {
	t.Helper()
	client, err := newJarClient()
	if err != nil {
		t.Fatal(err)
	}
	code, _ := postForm(t, client, baseURL+"/login", url.Values{"email": {email}, "password": {password}})
	if code != http.StatusSeeOther {
		t.Fatalf("login of %s: status %d", email, code)
	}
	return client
}

func TestSignupValidatesAndStartsSession(t *testing.T) {
	st := newWebTestStore(t)
	ts := rawServer(t, web.Config{Store: st})
	client, err := newJarClient()
	if err != nil {
		t.Fatal(err)
	}

	bad := []struct {
		name, email, password, confirm string
	}{
		{"invalid e-mail", "nao-e-email", "senha-bem-longa", "senha-bem-longa"},
		{"short password", "a@example.com", "curta", "curta"},
		{"too long for bcrypt", "a@example.com", strings.Repeat("x", 73), strings.Repeat("x", 73)},
		{"different confirmation", "a@example.com", "senha-bem-longa", "outra-senha-longa"},
	}
	for _, c := range bad {
		code, _ := postForm(t, client, ts.URL+"/signup", url.Values{"email": {c.email}, "password": {c.password}, "confirm": {c.confirm}})
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", c.name, code)
		}
	}
	if _, err := st.GetUserByEmail(context.Background(), "a@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an invalid signup created an account: %v", err)
	}

	form := url.Values{"email": {" Nova@Example.com "}, "password": {"senha-bem-longa"}, "confirm": {"senha-bem-longa"}}
	resp, err := client.PostForm(ts.URL+"/signup", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("signup: status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	user, err := st.GetUserByEmail(context.Background(), "nova@example.com")
	if err != nil {
		t.Fatalf("e-mail not normalized: %v", err)
	}
	if strings.Contains(user.PasswordHash, "senha-bem-longa") {
		t.Fatal("password stored in clear text")
	}
	if cost, err := bcrypt.Cost([]byte(user.PasswordHash)); err != nil || cost < 12 {
		t.Errorf("bcrypt cost %d, err %v", cost, err)
	}

	if code, body := getPage(t, client, ts.URL+"/account"); code != http.StatusOK || !strings.Contains(body, "nova@example.com") {
		t.Errorf("account page after signup: status %d", code)
	}

	other, err := newJarClient()
	if err != nil {
		t.Fatal(err)
	}
	form.Set("email", "NOVA@example.com")
	if code, _ := postForm(t, other, ts.URL+"/signup", form); code != http.StatusConflict {
		t.Errorf("repeated e-mail: status %d, want 409", code)
	}
}

func TestLoginLogoutAndExpiry(t *testing.T) {
	st := newWebTestStore(t)
	createUser(t, st, testEmail, "senha-certa-aqui")
	var mu sync.Mutex
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	ts := rawServer(t, web.Config{Store: st, Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}})
	client, err := newJarClient()
	if err != nil {
		t.Fatal(err)
	}

	assertRedirectsToLogin := func(when string) {
		t.Helper()
		resp, err := client.Get(ts.URL + "/account")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
			t.Errorf("%s: status %d, location %q", when, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	assertRedirectsToLogin("before login")

	_, wrongBody := postForm(t, client, ts.URL+"/login", url.Values{"email": {testEmail}, "password": {"errada"}})
	code, unknownBody := postForm(t, client, ts.URL+"/login", url.Values{"email": {"ninguem@example.com"}, "password": {"errada"}})
	if code != http.StatusUnauthorized {
		t.Errorf("unknown e-mail: status %d, want 401", code)
	}
	if !strings.Contains(wrongBody, "E-mail ou senha incorretos") || !strings.Contains(unknownBody, "E-mail ou senha incorretos") {
		t.Error("wrong password and unknown e-mail must give the same message")
	}
	assertRedirectsToLogin("after failed logins")

	resp, err := client.PostForm(ts.URL+"/login", url.Values{"email": {"DONO@example.com"}, "password": {"senha-certa-aqui"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: status %d", resp.StatusCode)
	}
	if code, _ := getPage(t, client, ts.URL+"/account"); code != http.StatusOK {
		t.Errorf("account after login: status %d", code)
	}

	token := pageCSRFToken(t, client, ts.URL)
	if code, _ := postForm(t, client, ts.URL+"/logout", url.Values{"csrf_token": {token}}); code != http.StatusSeeOther {
		t.Errorf("logout: status %d", code)
	}
	assertRedirectsToLogin("after logout")

	if code, _ := postForm(t, client, ts.URL+"/login", url.Values{"email": {testEmail}, "password": {"senha-certa-aqui"}}); code != http.StatusSeeOther {
		t.Fatalf("second login: status %d", code)
	}
	mu.Lock()
	now = now.Add(13 * time.Hour)
	mu.Unlock()
	assertRedirectsToLogin("after expiry")
}

func TestSessionCookieIsSecureBehindHTTPSProxy(t *testing.T) {
	st := newWebTestStore(t)
	ts := loginServer(t, st, "senha-certa-aqui")
	form := url.Values{"email": {testEmail}, "password": {"senha-certa-aqui"}}.Encode()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/login", strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie = %+v", cookies)
	}
}

func TestSignupIsLimitedPerClient(t *testing.T) {
	st := newWebTestStore(t)
	ts := rawServer(t, web.Config{Store: st})
	client, err := newJarClient()
	if err != nil {
		t.Fatal(err)
	}
	var last int
	for i := range 6 {
		form := url.Values{"email": {fmt.Sprintf("u%d@example.com", i)}, "password": {"senha-bem-longa"}, "confirm": {"senha-bem-longa"}}
		last, _ = postForm(t, client, ts.URL+"/signup", form)
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("sixth signup: status %d, want 429", last)
	}
	if _, err := st.GetUserByEmail(context.Background(), "u5@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the blocked signup created an account")
	}
}

func TestEntryFormsRejectForeignOrigin(t *testing.T) {
	st := newWebTestStore(t)
	ts := loginServer(t, st, "senha-certa-aqui")
	for _, path := range []string{"/login", "/signup"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader("email=x"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://mal.example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s from a foreign origin: status %d, want 403", path, resp.StatusCode)
		}
	}
}

type ownedFixture struct {
	plant  domain.Plant
	device domain.Device
	alert  store.Alert
	report store.Report
}

func seedOwned(t *testing.T, st *store.Store, userID int64, ext, label string) ownedFixture {
	t.Helper()
	ctx := context.Background()
	plant, device, _ := seedPlantWithDevices(t, st, ext, "Usina "+label)
	if userID != 0 {
		if err := st.ClaimSource(ctx, plant.SourceID, userID); err != nil {
			t.Fatal(err)
		}
	}
	alert := store.Alert{PlantID: plant.ID, DeviceID: &device.ID, Rule: "zero_generation", Severity: "critical", Status: "open",
		DedupeKey: ext, Title: "Alerta " + label, FirstDate: "2026-05-01", LastDate: "2026-05-01"}
	if err := st.SaveAlert(ctx, &alert); err != nil {
		t.Fatal(err)
	}
	report, err := st.InsertReport(ctx, store.Report{PlantID: &plant.ID, Kind: "monthly", PeriodStart: "2026-05-01", PeriodEnd: "2026-05-31",
		Status: "draft", Author: "system", Title: "Relatório " + label, BodyMD: "corpo", DataJSON: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	return ownedFixture{plant: plant, device: device, alert: alert, report: report}
}

// Each route is written as in server.go, so that a new route without a check
// here fails TestEveryRouteHasAnIsolationCheck.
type routeCheck struct {
	route string
	path  func(f ownedFixture) string
	form  func(f ownedFixture) url.Values
}

func idPath(format string, id func(f ownedFixture) int64) func(f ownedFixture) string {
	return func(f ownedFixture) string { return fmt.Sprintf(format, id(f)) }
}

var (
	plantID  = func(f ownedFixture) int64 { return f.plant.ID }
	deviceID = func(f ownedFixture) int64 { return f.device.ID }
	alertID  = func(f ownedFixture) int64 { return f.alert.ID }
	reportID = func(f ownedFixture) int64 { return f.report.ID }
)

func fixedPath(p string) func(f ownedFixture) string {
	return func(ownedFixture) string { return p }
}

var isolatedRoutes = []routeCheck{
	{route: "GET /plants/{id}", path: idPath("/plants/%d", plantID)},
	{route: "GET /api/v1/plants/{id}/series", path: idPath("/api/v1/plants/%d/series?from=2026-05-01&to=2026-05-02", plantID)},
	{route: "GET /alerts/{id}", path: idPath("/alerts/%d", alertID)},
	{route: "GET /reports/{id}", path: idPath("/reports/%d", reportID)},
	{route: "POST /alerts/{id}/ack", path: idPath("/alerts/%d/ack", alertID)},
	{route: "POST /alerts/{id}/resolve", path: idPath("/alerts/%d/resolve", alertID)},
	{route: "POST /reports/{id}/finalize", path: idPath("/reports/%d/finalize", reportID)},
	{route: "POST /manage/plants/{id}", path: idPath("/manage/plants/%d", plantID), form: func(ownedFixture) url.Values {
		return url.Values{"name": {"Nome alterado"}, "timezone": {"America/Manaus"}}
	}},
	{route: "POST /manage/devices/{id}", path: idPath("/manage/devices/%d", deviceID), form: func(ownedFixture) url.Values {
		return url.Values{"capacity_kw": {"99"}}
	}},
	{route: "POST /manage/rules", path: fixedPath("/manage/rules"), form: func(f ownedFixture) url.Values {
		return url.Values{"rule": {"zero_generation"}, "plant_id": {fmt.Sprint(f.plant.ID)}, "params_json": {"{}"}}
	}},
	{route: "POST /manage/pipeline/run", path: fixedPath("/manage/pipeline/run"), form: func(f ownedFixture) url.Values {
		return url.Values{"plant_id": {fmt.Sprint(f.plant.ID)}}
	}},
	{route: "POST /reports/generate", path: fixedPath("/reports/generate"), form: func(f ownedFixture) url.Values {
		return url.Values{"plant_id": {fmt.Sprint(f.plant.ID)}, "kind": {"period"}, "from": {"2026-05-01"}, "to": {"2026-05-02"}}
	}},
}

// These show only the data of the logged-in account, or none at all, and are
// checked by the tests below. They receive no ID from the visitor.
var routesWithoutID = map[string]bool{
	"GET /static/":               true,
	"GET /login":                 true,
	"POST /login":                true,
	"GET /signup":                true,
	"POST /signup":               true,
	"POST /logout":               true,
	"GET /{$}":                   true,
	"GET /alerts":                true,
	"GET /reports":               true,
	"GET /manage":                true,
	"GET /account":               true,
	"POST /account/token":        true,
	"POST /account/token/remove": true,
}

func TestEveryRouteHasAnIsolationCheck(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := map[string]bool{}
	for _, r := range isolatedRoutes {
		checked[r.route] = true
	}
	routes := regexp.MustCompile(`mux\.Handle(?:Func)?\("((?:GET|POST) [^"]+)"`).FindAllSubmatch(src, -1)
	if len(routes) < 20 {
		t.Fatalf("found only %d routes in server.go: did the format change?", len(routes))
	}
	for _, m := range routes {
		route := string(m[1])
		if !checked[route] && !routesWithoutID[route] {
			t.Errorf("route %q has no isolation check: add it to isolatedRoutes", route)
		}
	}
}

func TestAccountsCannotReachEachOthersData(t *testing.T) {
	st := newWebTestStore(t)
	ctx := context.Background()
	ts := rawServer(t, web.Config{Store: st})

	alice := createUser(t, st, "alice@example.com", "senha-da-alice")
	createUser(t, st, "bob@example.com", "senha-do-bob")
	target := seedOwned(t, st, alice.ID, "ALICE", "Alice")

	bob := loggedInClient(t, ts.URL, "bob@example.com", "senha-do-bob")
	bobToken := pageCSRFToken(t, bob, ts.URL)

	request := func(c *http.Client, token string, check routeCheck) int {
		t.Helper()
		method, _, _ := strings.Cut(check.route, " ")
		if method == http.MethodGet {
			code, _ := getPage(t, c, ts.URL+check.path(target))
			return code
		}
		form := url.Values{}
		if check.form != nil {
			form = check.form(target)
		}
		form.Set("csrf_token", token)
		code, _ := postForm(t, c, ts.URL+check.path(target), form)
		return code
	}

	for _, check := range isolatedRoutes {
		if code := request(bob, bobToken, check); code != http.StatusNotFound {
			t.Errorf("%s by another account: status %d, want 404", check.route, code)
		}
	}

	// None of those attempts may have changed anything.
	plant, err := st.GetPlant(ctx, target.plant.ID)
	if err != nil || plant.Name != "Usina Alice" || plant.Timezone != target.plant.Timezone {
		t.Errorf("plant changed: %+v, err %v", plant, err)
	}
	device, err := st.GetDevice(ctx, target.device.ID)
	if err != nil || device.CapacityKW != nil {
		t.Errorf("device changed: %+v, err %v", device, err)
	}
	alert, err := st.GetAlert(ctx, target.alert.ID)
	if err != nil || alert.Status != "open" {
		t.Errorf("alert changed: %+v, err %v", alert, err)
	}
	report, err := st.GetReport(ctx, target.report.ID)
	if err != nil || report.Status != "draft" {
		t.Errorf("report changed: %+v, err %v", report, err)
	}
	rules, err := st.GetRuleConfigs(ctx, target.plant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed := rules["zero_generation"]; changed {
		t.Error("rule config created by another account")
	}
	reports, err := st.ListReports(ctx, store.ReportFilter{PlantID: &target.plant.ID})
	if err != nil || len(reports) != 1 {
		t.Errorf("reports of the plant: %d, err %v", len(reports), err)
	}
	time.Sleep(50 * time.Millisecond)
	if runs, _ := st.ListPipelineRuns(ctx, &target.plant.ID, 5); len(runs) != 0 {
		t.Error("pipeline ran for another account's plant")
	}

	// Positive control: the same requests work for the owner, so the 404s above
	// come from the ownership check and not from a broken request.
	owner := loggedInClient(t, ts.URL, "alice@example.com", "senha-da-alice")
	ownerToken := pageCSRFToken(t, owner, ts.URL)
	for _, check := range isolatedRoutes {
		if code := request(owner, ownerToken, check); code == http.StatusNotFound || code == http.StatusForbidden {
			t.Errorf("%s by the owner: status %d", check.route, code)
		}
	}
}

func TestListPagesShowOnlyTheAccountsData(t *testing.T) {
	st := newWebTestStore(t)
	ts := rawServer(t, web.Config{Store: st})

	alice := createUser(t, st, "alice@example.com", "senha-da-alice")
	bob := createUser(t, st, "bob@example.com", "senha-do-bob")
	aliceData := seedOwned(t, st, alice.ID, "ALICE", "Alice")
	seedOwned(t, st, bob.ID, "BOB", "Bob")
	seedOwned(t, st, 0, "SEMDONO", "Sem Dono")
	if _, err := st.DB().Exec(`INSERT INTO pipeline_runs (plant_id, local_date, status, steps_json, started_at, finished_at)
		VALUES (?, '2026-05-01', 'success', '[]', '2026-05-01T05:00:00Z', '2026-05-01T05:01:00Z')`, aliceData.plant.ID); err != nil {
		t.Fatal(err)
	}

	client := loggedInClient(t, ts.URL, "bob@example.com", "senha-do-bob")
	pages := []string{"/", "/alerts", fmt.Sprintf("/alerts?plant_id=%d", aliceData.plant.ID), "/reports", "/manage", "/account"}
	for _, page := range pages {
		code, body := getPage(t, client, ts.URL+page)
		if code != http.StatusOK {
			t.Errorf("%s: status %d", page, code)
		}
		for _, foreign := range []string{"Usina Alice", "Alerta Alice", "Relatório Alice", "source_ALICE", "Usina Sem Dono", "Alerta Sem Dono", "source_SEMDONO"} {
			if strings.Contains(body, foreign) {
				t.Errorf("%s shows %q to another account", page, foreign)
			}
		}
	}
	if _, body := getPage(t, client, ts.URL+"/manage"); !strings.Contains(body, "Nenhuma execução registrada") {
		t.Error("/manage shows pipeline runs of another account")
	}
	for page, want := range map[string]string{"/": "Usina Bob", "/alerts": "Alerta Bob", "/reports": "Relatório Bob", "/manage": "source_BOB"} {
		if _, body := getPage(t, client, ts.URL+page); !strings.Contains(body, want) {
			t.Errorf("%s does not show the account's own %q", page, want)
		}
	}
}

func TestReportWithoutPlantIsHiddenFromAccounts(t *testing.T) {
	st := newWebTestStore(t)
	ts := rawServer(t, web.Config{Store: st})
	createUser(t, st, "bob@example.com", "senha-do-bob")
	report, err := st.InsertReport(context.Background(), store.Report{Kind: "monthly", PeriodStart: "2026-05-01", PeriodEnd: "2026-05-31",
		Status: "draft", Author: "system", Title: "Todas as usinas", BodyMD: "corpo", DataJSON: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	client := loggedInClient(t, ts.URL, "bob@example.com", "senha-do-bob")
	if code, _ := getPage(t, client, fmt.Sprintf("%s/reports/%d", ts.URL, report.ID)); code != http.StatusNotFound {
		t.Errorf("report without a plant: status %d, want 404", code)
	}
	if _, body := getPage(t, client, ts.URL+"/reports"); strings.Contains(body, "Todas as usinas") {
		t.Error("report without a plant appears in the list")
	}
}

func TestRoutesRequireLogin(t *testing.T) {
	st := newWebTestStore(t)
	ts := rawServer(t, web.Config{Store: st})
	target := seedOwned(t, st, 0, "X", "X")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	all := append([]routeCheck{}, isolatedRoutes...)
	for _, route := range []string{"GET /", "GET /alerts", "GET /reports", "GET /manage", "GET /account", "POST /account/token", "POST /account/token/remove"} {
		_, path, _ := strings.Cut(route, " ")
		all = append(all, routeCheck{route: route, path: fixedPath(path)})
	}
	for _, check := range all {
		method, _, _ := strings.Cut(check.route, " ")
		req, err := http.NewRequest(method, ts.URL+check.path(target), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without login: status %d", check.route, resp.StatusCode)
		}
	}
}

type fakeGrowatt struct {
	mu          sync.Mutex
	plants      map[string][]string
	rateLimited bool
}

type fakeGrowattConn struct {
	f     *fakeGrowatt
	token string
}

func (c *fakeGrowattConn) Kind() string { return "growatt" }
func (c *fakeGrowattConn) Discovery() []connector.Task {
	return []connector.Task{{Endpoint: "plant/list", LimitKey: "plant/list", Priority: 20, Refreshable: true}}
}
func (c *fakeGrowattConn) DailyTasks(domain.Plant, []domain.Device, string) []connector.Task {
	return nil
}
func (c *fakeGrowattConn) Execute(context.Context, connector.Task) (connector.Result, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	if c.f.rateLimited {
		return connector.Result{}, &connector.RateLimitError{RetryAfter: time.Minute}
	}
	ids, ok := c.f.plants[c.token]
	if !ok {
		return connector.Result{}, errors.New("growatt error 10011")
	}
	res := connector.Result{Raw: []byte(`{}`)}
	for _, id := range ids {
		res.Plants = append(res.Plants, connector.PlantInfo{ExternalID: id, Name: "Usina " + id, Timezone: "America/Sao_Paulo"})
	}
	return res, nil
}

func tokenServer(t *testing.T, st *store.Store, f *fakeGrowatt) (*httptest.Server, *ingest.Manager) {
	t.Helper()
	box, err := secrets.New("chave-de-teste-com-mais-de-32-caracteres")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr := ingest.NewManager(ctx, st, box, nil, ingest.WithConnectorFactory(func(_, token string) connector.Connector {
		return &fakeGrowattConn{f: f, token: token}
	}))
	t.Cleanup(func() {
		cancel()
		mgr.Wait()
	})
	return rawServer(t, web.Config{Store: st, Tokens: mgr}), mgr
}

func TestAccountTokenStartsSyncAndIsNeverShown(t *testing.T) {
	const token = "token-muito-secreto-123"
	st := newWebTestStore(t)
	ctx := context.Background()
	f := &fakeGrowatt{plants: map[string][]string{token: {"P1"}}}
	ts, mgr := tokenServer(t, st, f)
	createUser(t, st, "alice@example.com", "senha-da-alice")
	client := loggedInClient(t, ts.URL, "alice@example.com", "senha-da-alice")

	if _, body := getPage(t, client, ts.URL+"/account"); !strings.Contains(body, "Sem token") {
		t.Error("new account should show that it has no token")
	}
	if _, body := getPage(t, client, ts.URL+"/"); !strings.Contains(body, "/account") {
		t.Error("empty overview should point to the account page")
	}

	csrf := pageCSRFToken(t, client, ts.URL)

	if code, _ := postForm(t, client, ts.URL+"/account/token", url.Values{"token": {token}}); code != http.StatusForbidden {
		t.Errorf("no CSRF token: status %d, want 403", code)
	}

	code, body := postForm(t, client, ts.URL+"/account/token", url.Values{"csrf_token": {csrf}, "token": {"token-errado-456"}})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("rejected token: status %d, want 422", code)
	}
	if strings.Contains(body, "token-errado-456") {
		t.Error("the page repeats a rejected token")
	}

	f.mu.Lock()
	f.rateLimited = true
	f.mu.Unlock()
	if code, _ := postForm(t, client, ts.URL+"/account/token", url.Values{"csrf_token": {csrf}, "token": {token}}); code != http.StatusTooManyRequests {
		t.Errorf("provider rate limit: status %d, want 429", code)
	}
	f.mu.Lock()
	f.rateLimited = false
	f.mu.Unlock()

	code, _ = postForm(t, client, ts.URL+"/account/token", url.Values{"csrf_token": {csrf}, "token": {"  " + token + "\n"}})
	if code != http.StatusSeeOther {
		t.Fatalf("valid token: status %d, want 303", code)
	}

	code, body = getPage(t, client, ts.URL+"/account?ok=conectado")
	if code != http.StatusOK || !strings.Contains(body, "Sincronizando") || !strings.Contains(body, "A sincronização começou") {
		t.Errorf("account after saving: status %d", code)
	}
	for _, page := range []string{"/", "/manage", "/account"} {
		_, pageBody := getPage(t, client, ts.URL+page)
		if strings.Contains(pageBody, token) {
			t.Errorf("%s shows the token", page)
		}
	}
	if _, overview := getPage(t, client, ts.URL+"/"); !strings.Contains(overview, "Usina P1") {
		t.Error("the plant did not appear right after saving the token")
	}

	src, err := st.GetSourceByName(ctx, "growatt-conta-1")
	if err != nil {
		t.Fatal(err)
	}
	if !mgr.Collecting(src.ID) {
		t.Error("no collector running")
	}
	var dump string
	if err := st.DB().QueryRow(`SELECT credential_enc || credential_env || config_json FROM sources WHERE id = ?`, src.ID).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, token) {
		t.Error("token stored in clear text")
	}

	// Another account cannot attach the same plants.
	createUser(t, st, "bob@example.com", "senha-do-bob")
	bob := loggedInClient(t, ts.URL, "bob@example.com", "senha-do-bob")
	bobCSRF := pageCSRFToken(t, bob, ts.URL)
	if code, _ := postForm(t, bob, ts.URL+"/account/token", url.Values{"csrf_token": {bobCSRF}, "token": {token}}); code != http.StatusConflict {
		t.Errorf("same plants on another account: status %d, want 409", code)
	}

	if code, _ := postForm(t, client, ts.URL+"/account/token/remove", url.Values{"csrf_token": {csrf}}); code != http.StatusSeeOther {
		t.Errorf("remove token: status %d", code)
	}
	if mgr.Collecting(src.ID) {
		t.Error("still collecting after removal")
	}
	if _, body := getPage(t, client, ts.URL+"/account"); !strings.Contains(body, "Sem token") {
		t.Error("account should show no token after removal")
	}
	if _, overview := getPage(t, client, ts.URL+"/"); !strings.Contains(overview, "Usina P1") {
		t.Error("removing the token must keep the data")
	}
}

func TestAccountTokenAttemptsAreLimited(t *testing.T) {
	st := newWebTestStore(t)
	ts, _ := tokenServer(t, st, &fakeGrowatt{plants: map[string][]string{}})
	createUser(t, st, "alice@example.com", "senha-da-alice")
	client := loggedInClient(t, ts.URL, "alice@example.com", "senha-da-alice")
	csrf := pageCSRFToken(t, client, ts.URL)

	var last int
	for range 6 {
		last, _ = postForm(t, client, ts.URL+"/account/token", url.Values{"csrf_token": {csrf}, "token": {"errado"}})
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("sixth wrong token: status %d, want 429", last)
	}
}

func TestAccountWithoutTokenManagerExplainsIt(t *testing.T) {
	st := newWebTestStore(t)
	ts := rawServer(t, web.Config{Store: st})
	createUser(t, st, "alice@example.com", "senha-da-alice")
	client := loggedInClient(t, ts.URL, "alice@example.com", "senha-da-alice")
	csrf := pageCSRFToken(t, client, ts.URL)
	if code, _ := postForm(t, client, ts.URL+"/account/token", url.Values{"csrf_token": {csrf}, "token": {"x"}}); code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", code)
	}
}

// With a changed encryption key the saved token cannot start a collector. The
// page must say so instead of claiming that it is syncing.
func TestAccountShowsWhenSavedTokenIsNotCollecting(t *testing.T) {
	st := newWebTestStore(t)
	ts, _ := tokenServer(t, st, &fakeGrowatt{plants: map[string][]string{}})
	user := createUser(t, st, "alice@example.com", "senha-da-alice")
	if _, err := st.EnsureSource(context.Background(), domain.Source{
		Kind: "growatt", Name: "fonte", CredentialEnc: "cifrado-com-outra-chave", Enabled: true, UserID: &user.ID,
	}); err != nil {
		t.Fatal(err)
	}
	client := loggedInClient(t, ts.URL, "alice@example.com", "senha-da-alice")
	_, body := getPage(t, client, ts.URL+"/account")
	if strings.Contains(body, "Sincronizando") || !strings.Contains(body, "Informe o token de novo") {
		t.Error("the page claims to be syncing while no collector runs")
	}
}

func TestSignupCanBeClosed(t *testing.T) {
	st := newWebTestStore(t)
	createUser(t, st, testEmail, "senha-certa-aqui")
	ts := rawServer(t, web.Config{Store: st, SignupClosed: true})
	client, err := newJarClient()
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Get(ts.URL + "/signup")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Errorf("GET /signup: status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	form := url.Values{"email": {"nova@example.com"}, "password": {"senha-bem-longa"}, "confirm": {"senha-bem-longa"}}
	code, body := postForm(t, client, ts.URL+"/signup", form)
	if code != http.StatusForbidden || !strings.Contains(body, "Novos cadastros estão desativados") {
		t.Errorf("POST /signup: status %d", code)
	}
	if _, err := st.GetUserByEmail(context.Background(), "nova@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a closed signup created an account: %v", err)
	}

	if _, loginPage := getPage(t, client, ts.URL+"/login"); strings.Contains(loginPage, `href="/signup"`) {
		t.Error("the login page offers a signup that is closed")
	}
	if code, _ := postForm(t, client, ts.URL+"/login", url.Values{"email": {testEmail}, "password": {"senha-certa-aqui"}}); code != http.StatusSeeOther {
		t.Errorf("existing account cannot log in: status %d", code)
	}
}

func TestLoginPageOffersSignupWhenOpen(t *testing.T) {
	ts := rawServer(t, web.Config{Store: newWebTestStore(t)})
	if _, body := getPage(t, http.DefaultClient, ts.URL+"/login"); !strings.Contains(body, `href="/signup"`) {
		t.Error("the login page does not offer signup")
	}
}
