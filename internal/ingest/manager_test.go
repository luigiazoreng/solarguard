package ingest_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/ingest"
	"github.com/luigiazoreng/solarguard/internal/secrets"
	"github.com/luigiazoreng/solarguard/internal/store"
)

const testKey = "chave-de-teste-com-mais-de-32-caracteres"

// fakeProvider answers like Growatt: each token lists its own plants.
type fakeProvider struct {
	mu          sync.Mutex
	plants      map[string][]string
	rateLimited map[string]bool
	listings    map[string]int
}

func (f *fakeProvider) listingsOf(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listings[token]
}

type fakeConn struct {
	p     *fakeProvider
	token string
}

func (c *fakeConn) Kind() string { return "growatt" }
func (c *fakeConn) Discovery() []connector.Task {
	return []connector.Task{{Endpoint: "plant/list", LimitKey: "plant/list", MinInterval: 5 * time.Minute, Priority: 20, Refreshable: true}}
}
func (c *fakeConn) DailyTasks(domain.Plant, []domain.Device, string) []connector.Task { return nil }
func (c *fakeConn) Execute(_ context.Context, task connector.Task) (connector.Result, error) {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	if c.p.rateLimited[c.token] {
		return connector.Result{}, &connector.RateLimitError{RetryAfter: 5 * time.Minute}
	}
	ids, ok := c.p.plants[c.token]
	if !ok {
		return connector.Result{}, errors.New("growatt error 10011: permission denied")
	}
	c.p.listings[c.token]++
	res := connector.Result{Raw: []byte(`{}`)}
	for _, id := range ids {
		res.Plants = append(res.Plants, connector.PlantInfo{ExternalID: id, Name: "Planta " + id, Timezone: "America/Sao_Paulo"})
	}
	return res, nil
}

type managerEnv struct {
	st       *store.Store
	provider *fakeProvider
	mgr      *ingest.Manager
	box      *secrets.Box
	cancel   context.CancelFunc
}

func newManagerEnv(t *testing.T) *managerEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{plants: map[string][]string{}, rateLimited: map[string]bool{}, listings: map[string]int{}}
	env := &managerEnv{st: st, provider: p, box: box, cancel: cancel}
	env.mgr = env.newManager(ctx)
	t.Cleanup(func() {
		cancel()
		env.mgr.Wait()
		_ = st.Close()
	})
	return env
}

func (e *managerEnv) newManager(ctx context.Context) *ingest.Manager {
	return ingest.NewManager(ctx, e.st, e.box, nil, ingest.WithConnectorFactory(func(_, token string) connector.Connector {
		return &fakeConn{p: e.provider, token: token}
	}))
}

func (e *managerEnv) user(t *testing.T, email string) int64 {
	t.Helper()
	u, err := e.st.CreateUser(context.Background(), email, "hash")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestConnectStoresEncryptedTokenAndCollectsImmediately(t *testing.T) {
	e := newManagerEnv(t)
	ctx := context.Background()
	uid := e.user(t, "a@example.com")
	e.provider.plants["token-secreto"] = []string{"P1", "P2"}

	res, err := e.mgr.Connect(ctx, uid, "token-secreto")
	if err != nil {
		t.Fatal(err)
	}
	if res.PlantsFound != 2 || res.Adopted {
		t.Errorf("result = %+v", res)
	}

	plants, err := e.st.ListPlantsForUser(ctx, uid)
	if err != nil || len(plants) != 2 {
		t.Fatalf("plants right after connecting: %d, err %v", len(plants), err)
	}
	if !e.mgr.Collecting(res.Source.ID) {
		t.Error("no collector running after connecting")
	}
	if n := e.provider.listingsOf("token-secreto"); n != 1 {
		t.Errorf("provider listed plants %d times: the validation must feed the queue", n)
	}

	var enc, env string
	if err := e.st.DB().QueryRowContext(ctx, `SELECT credential_enc, credential_env FROM sources WHERE id = ?`, res.Source.ID).Scan(&enc, &env); err != nil {
		t.Fatal(err)
	}
	if enc == "" || strings.Contains(enc, "token-secreto") || env != "" {
		t.Errorf("stored credential enc=%q env=%q", enc, env)
	}
	if got, err := e.box.Open(enc); err != nil || got != "token-secreto" {
		t.Errorf("stored token does not decrypt: %q, %v", got, err)
	}

	// The queue must not repeat the listing the validation already did.
	time.Sleep(50 * time.Millisecond)
	if n := e.provider.listingsOf("token-secreto"); n != 1 {
		t.Errorf("after the collector started, provider listed plants %d times, want 1", n)
	}
}

func TestConnectRejectedTokenStoresNothing(t *testing.T) {
	e := newManagerEnv(t)
	ctx := context.Background()
	uid := e.user(t, "a@example.com")

	_, err := e.mgr.Connect(ctx, uid, "token-invalido")
	if !errors.Is(err, ingest.ErrTokenRejected) {
		t.Fatalf("err = %v, want ErrTokenRejected", err)
	}
	if strings.Contains(err.Error(), "token-invalido") {
		t.Error("error message contains the token")
	}
	sources, err := e.st.ListSources(ctx)
	if err != nil || len(sources) != 0 {
		t.Errorf("sources after a rejected token: %d, err %v", len(sources), err)
	}

	if _, err := e.mgr.Connect(ctx, uid, ""); !errors.Is(err, ingest.ErrTokenRejected) {
		t.Errorf("empty token: err = %v", err)
	}
}

func TestConnectRateLimitedStoresNothing(t *testing.T) {
	e := newManagerEnv(t)
	ctx := context.Background()
	uid := e.user(t, "a@example.com")
	e.provider.rateLimited["token"] = true

	_, err := e.mgr.Connect(ctx, uid, "token")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if sources, _ := e.st.ListSources(ctx); len(sources) != 0 {
		t.Errorf("sources after a rate limited check: %d", len(sources))
	}
}

// An imported database has a source with history and no owner. Only a token that
// lists those plants can take it, and it must keep its history.
func TestConnectAdoptsUnownedSourceOnlyWithProofOfAccess(t *testing.T) {
	e := newManagerEnv(t)
	ctx := context.Background()
	legacy, err := e.st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "growatt-principal", CredentialEnv: "GROWATT_TOKEN", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	plant, err := e.st.UpsertPlant(ctx, domain.Plant{SourceID: legacy.ID, ExternalID: "HIST", Name: "Histórica", Timezone: "America/Sao_Paulo"})
	if err != nil {
		t.Fatal(err)
	}

	stranger := e.user(t, "estranho@example.com")
	e.provider.plants["token-estranho"] = []string{"OUTRA"}
	if _, err := e.mgr.Connect(ctx, stranger, "token-estranho"); err != nil {
		t.Fatal(err)
	}
	if owned, _ := e.st.PlantOwnedBy(ctx, plant.ID, stranger); owned {
		t.Fatal("a token that does not list the plant took its history")
	}

	owner := e.user(t, "dono@example.com")
	e.provider.plants["token-dono"] = []string{"HIST", "NOVA"}
	res, err := e.mgr.Connect(ctx, owner, "token-dono")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Adopted || res.Source.ID != legacy.ID {
		t.Errorf("result = %+v, want adoption of source %d", res, legacy.ID)
	}
	if owned, _ := e.st.PlantOwnedBy(ctx, plant.ID, owner); !owned {
		t.Error("owner did not receive the plant")
	}
	if res.Source.CredentialEnv != "" {
		t.Errorf("adopted source still depends on env %q", res.Source.CredentialEnv)
	}
	plants, _ := e.st.ListPlantsForUser(ctx, owner)
	if len(plants) != 2 {
		t.Errorf("plants of the owner: %d, want the historic and the new one", len(plants))
	}
}

func TestConnectRefusesPlantsOfAnotherAccount(t *testing.T) {
	e := newManagerEnv(t)
	ctx := context.Background()
	first := e.user(t, "a@example.com")
	second := e.user(t, "b@example.com")
	e.provider.plants["token"] = []string{"P1"}

	if _, err := e.mgr.Connect(ctx, first, "token"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.Connect(ctx, second, "token"); !errors.Is(err, ingest.ErrPlantsOwned) {
		t.Fatalf("err = %v, want ErrPlantsOwned", err)
	}
	if plants, _ := e.st.ListPlantsForUser(ctx, second); len(plants) != 0 {
		t.Errorf("second account sees %d plants", len(plants))
	}
}

func TestReplacingTokenKeepsOneCollectorAndSource(t *testing.T) {
	e := newManagerEnv(t)
	ctx := context.Background()
	uid := e.user(t, "a@example.com")
	e.provider.plants["velho"] = []string{"P1"}
	e.provider.plants["novo"] = []string{"P1"}

	first, err := e.mgr.Connect(ctx, uid, "velho")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.mgr.Connect(ctx, uid, "novo")
	if err != nil {
		t.Fatal(err)
	}
	if first.Source.ID != second.Source.ID {
		t.Errorf("replacing the token created source %d", second.Source.ID)
	}
	if got, err := e.box.Open(second.Source.CredentialEnc); err != nil || got != "novo" {
		t.Errorf("stored token = %q, %v", got, err)
	}
}

func TestDisconnectStopsCollectingAndKeepsData(t *testing.T) {
	e := newManagerEnv(t)
	ctx := context.Background()
	uid := e.user(t, "a@example.com")
	e.provider.plants["token"] = []string{"P1"}
	res, err := e.mgr.Connect(ctx, uid, "token")
	if err != nil {
		t.Fatal(err)
	}

	if err := e.mgr.Disconnect(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if e.mgr.Collecting(res.Source.ID) {
		t.Error("still collecting after removing the token")
	}
	src, err := e.st.GetSource(ctx, res.Source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src.CredentialEnc != "" {
		t.Error("token still stored after removal")
	}
	if plants, _ := e.st.ListPlantsForUser(ctx, uid); len(plants) != 1 {
		t.Errorf("plants after removal: %d, want the data kept", len(plants))
	}
}

// After a restart, the saved token must resume collecting with no environment variable.
func TestStartAllResumesFromSavedToken(t *testing.T) {
	e := newManagerEnv(t)
	ctx := context.Background()
	uid := e.user(t, "a@example.com")
	e.provider.plants["token"] = []string{"P1"}
	res, err := e.mgr.Connect(ctx, uid, "token")
	if err != nil {
		t.Fatal(err)
	}
	e.mgr.Wait()

	restarted := e.newManager(ctx)
	t.Cleanup(restarted.Wait)
	if err := restarted.StartAll(); err != nil {
		t.Fatal(err)
	}
	if !restarted.Collecting(res.Source.ID) {
		t.Error("saved token did not resume collecting")
	}
}

func TestStartAllSkipsSourceWithoutToken(t *testing.T) {
	e := newManagerEnv(t)
	ctx := context.Background()
	t.Setenv("TOKEN_AUSENTE", "")
	src, err := e.st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "sem-token", CredentialEnv: "TOKEN_AUSENTE", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.StartAll(); err != nil {
		t.Fatal(err)
	}
	if e.mgr.Collecting(src.ID) {
		t.Error("collector started without a token")
	}
}

type dailyConn struct{ fakeConn }

func (c *dailyConn) DailyTasks(_ domain.Plant, _ []domain.Device, date string) []connector.Task {
	return []connector.Task{{Endpoint: "daily", LimitKey: "daily:" + date}}
}

// The listing reveals the devices of each plant, which keeps discovery pending
// when the scheduler first runs. The first readings must not wait 15 minutes.
func TestSchedulerCollectsDaysAsSoonAsDiscoveryEnds(t *testing.T) {
	e := newManagerEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src, err := e.st.EnsureSource(ctx, domain.Source{Kind: "growatt", Name: "fonte", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertPlant(ctx, domain.Plant{SourceID: src.ID, ExternalID: "P1", Name: "P1", Timezone: "America/Sao_Paulo"}); err != nil {
		t.Fatal(err)
	}
	noon := time.Date(2026, 5, 10, 15, 0, 0, 0, time.UTC)
	if err := e.st.EnqueueTask(ctx, src.ID, connector.Task{Endpoint: "device/list", LimitKey: "device/list?plant_id=P1"}, noon, false); err != nil {
		t.Fatal(err)
	}

	countDaily := func() int {
		t.Helper()
		var n int
		if err := e.st.DB().QueryRow(`SELECT count(*) FROM fetch_tasks WHERE task_key LIKE 'daily:%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	conn := &dailyConn{fakeConn{p: e.provider, token: "t"}}
	sched := ingest.NewScheduler(e.st, src.ID, conn, ingest.WithSchedulerNow(func() time.Time { return noon }), ingest.WithSchedulerPoll(5*time.Millisecond))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sched.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	time.Sleep(50 * time.Millisecond)
	if n := countDaily(); n != 0 {
		t.Fatalf("scheduled %d days while discovery was pending", n)
	}

	if _, err := e.st.DB().Exec(`UPDATE fetch_tasks SET status = 'done' WHERE task_key LIKE 'device/list%' OR task_key LIKE 'plant/list%'`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for countDaily() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("days were not scheduled after discovery ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
