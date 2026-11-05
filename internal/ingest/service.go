package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/luigiazoreng/solarguard/internal/connector"
	"github.com/luigiazoreng/solarguard/internal/connector/growatt"
	"github.com/luigiazoreng/solarguard/internal/domain"
	"github.com/luigiazoreng/solarguard/internal/secrets"
	"github.com/luigiazoreng/solarguard/internal/store"
)

const (
	kindGrowatt        = "growatt"
	defaultGrowattBase = "https://openapi.growatt.com"
)

var (
	// ErrNoConnector marks a source that has nothing to collect, such as an imported file.
	ErrNoConnector = errors.New("tipo de fonte sem conector de coleta")
	// ErrTokenRejected reports that the provider did not accept the token.
	ErrTokenRejected = errors.New("token recusado")
	// ErrPlantsOwned reports that another account already monitors these plants.
	ErrPlantsOwned = errors.New("plantas já vinculadas a outra conta")
)

// ConnectorFactory builds a provider connector from a base URL and a token.
type ConnectorFactory func(baseURL, token string) connector.Connector

func newGrowatt(baseURL, token string) connector.Connector {
	return growatt.New(baseURL, token, &http.Client{Timeout: 30 * time.Second})
}

// ResolveSourceConnector returns the configured connector for a source. A new
// provider is added here, in this switch. box may be nil when the source keeps
// its token in the environment.
func ResolveSourceConnector(src domain.Source, box *secrets.Box) (connector.Connector, error) {
	return resolve(src, box, newGrowatt)
}

func resolve(src domain.Source, box *secrets.Box, newConn ConnectorFactory) (connector.Connector, error) {
	switch src.Kind {
	case kindGrowatt:
		token, err := sourceToken(src, box)
		if err != nil {
			return nil, err
		}
		baseURL := defaultGrowattBase
		var cfg struct {
			BaseURL string `json:"base_url"`
		}
		if err := json.Unmarshal([]byte(src.ConfigJSON), &cfg); err == nil && cfg.BaseURL != "" {
			baseURL = cfg.BaseURL
		}
		return newConn(baseURL, token), nil
	default:
		return nil, fmt.Errorf("fonte %q, tipo %q: %w", src.Name, src.Kind, ErrNoConnector)
	}
}

func sourceToken(src domain.Source, box *secrets.Box) (string, error) {
	if src.CredentialEnc != "" {
		if box == nil {
			return "", fmt.Errorf("fonte %q: %w", src.Name, secrets.ErrNoKey)
		}
		token, err := box.Open(src.CredentialEnc)
		if err != nil {
			return "", fmt.Errorf("fonte %q: %w", src.Name, err)
		}
		return token, nil
	}
	if src.CredentialEnv == "" {
		return "", fmt.Errorf("fonte %q não tem token configurado", src.Name)
	}
	token := os.Getenv(src.CredentialEnv)
	if token == "" {
		return "", fmt.Errorf("variável de ambiente %q não definida", src.CredentialEnv)
	}
	return token, nil
}

// ConnectResult describes a token that was accepted.
type ConnectResult struct {
	Source      domain.Source
	PlantsFound int
	// Adopted is true when the token proved access to plants already in the
	// database, whose history now belongs to this account.
	Adopted bool
}

type collector struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager runs one collector per source and starts or stops them while the
// server is running, as accounts add or remove tokens.
type Manager struct {
	ctx     context.Context
	st      *store.Store
	box     *secrets.Box
	logger  *log.Logger
	newConn ConnectorFactory
	baseURL string

	mu         sync.Mutex // serializes token changes and guards collectors
	collectors map[int64]*collector
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithConnectorFactory replaces how connectors are built.
func WithConnectorFactory(f ConnectorFactory) ManagerOption {
	return func(m *Manager) { m.newConn = f }
}

// WithGrowattBaseURL sets the API address used for tokens added by accounts.
func WithGrowattBaseURL(baseURL string) ManagerOption {
	return func(m *Manager) {
		if baseURL != "" {
			m.baseURL = baseURL
		}
	}
}

// NewManager creates a Manager. Collectors stop when ctx ends.
func NewManager(ctx context.Context, st *store.Store, box *secrets.Box, logger *log.Logger, opts ...ManagerOption) *Manager {
	m := &Manager{
		ctx:        ctx,
		st:         st,
		box:        box,
		logger:     logger,
		newConn:    newGrowatt,
		baseURL:    defaultGrowattBase,
		collectors: make(map[int64]*collector),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// StartAll starts a collector for every enabled source that has a connector and
// a token. A source that cannot start logs why and does not affect the others.
func (m *Manager) StartAll() error {
	sources, err := m.st.ListSources(m.ctx)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, src := range sources {
		if !src.Enabled {
			continue
		}
		conn, err := resolve(src, m.box, m.newConn)
		if errors.Is(err, ErrNoConnector) {
			continue
		}
		if err != nil {
			m.logf("source %q (id %d): %v, skipping", src.Name, src.ID, err)
			continue
		}
		m.start(src, conn)
	}
	return nil
}

// Connect validates a token with the provider, stores it encrypted for the
// account and starts collecting. Nothing is stored unless the provider accepts
// the token.
func (m *Manager) Connect(ctx context.Context, userID int64, token string) (ConnectResult, error) {
	if m.box == nil {
		return ConnectResult{}, secrets.ErrNoKey
	}
	if token == "" {
		return ConnectResult{}, fmt.Errorf("%w: token vazio", ErrTokenRejected)
	}

	conn := m.newConn(m.baseURL, token)
	discovery := conn.Discovery()
	if len(discovery) == 0 {
		return ConnectResult{}, errors.New("conector sem tarefa de descoberta")
	}
	listing, err := conn.Execute(ctx, discovery[0])
	if errors.Is(err, connector.ErrRateLimited) {
		return ConnectResult{}, err
	}
	if err != nil {
		return ConnectResult{}, fmt.Errorf("%w: %v", ErrTokenRejected, err)
	}

	sealed, err := m.box.Seal(token)
	if err != nil {
		return ConnectResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	target, adopted, previous, err := m.chooseSource(ctx, userID, listing.Plants)
	if err != nil {
		return ConnectResult{}, err
	}

	m.stop(target.ID)
	if previous != nil {
		m.stop(previous.ID)
		if err := m.st.ClearSourceCredential(ctx, previous.ID); err != nil {
			return ConnectResult{}, err
		}
	}
	if err := m.st.SetSourceCredential(ctx, target.ID, sealed); err != nil {
		return ConnectResult{}, err
	}
	target, err = m.st.GetSource(ctx, target.ID)
	if err != nil {
		return ConnectResult{}, err
	}

	// The provider allows few listings, so the one that validated the token also
	// feeds the queue. Asking again would be refused for several minutes.
	if err := m.seed(ctx, target, conn, discovery[0], listing); err != nil {
		m.logf("source %q: seed discovery: %v", target.Name, err)
	}

	running, err := resolve(target, m.box, m.newConn)
	if err != nil {
		return ConnectResult{}, err
	}
	m.start(target, running)

	return ConnectResult{Source: target, PlantsFound: len(listing.Plants), Adopted: adopted}, nil
}

// chooseSource decides which source receives the token. Holding a token that
// lists a plant is the proof of access that allows adopting its unowned history.
func (m *Manager) chooseSource(ctx context.Context, userID int64, plants []connector.PlantInfo) (target domain.Source, adopted bool, previous *domain.Source, err error) {
	ids := make([]string, 0, len(plants))
	for _, p := range plants {
		ids = append(ids, p.ExternalID)
	}
	matches, err := m.st.SourcesWithPlants(ctx, kindGrowatt, ids)
	if err != nil {
		return domain.Source{}, false, nil, err
	}
	var unowned *domain.Source
	for i, src := range matches {
		switch {
		case src.UserID == nil:
			if unowned == nil {
				unowned = &matches[i]
			}
		case *src.UserID != userID:
			return domain.Source{}, false, nil, ErrPlantsOwned
		}
	}

	own, err := m.st.ListSourcesForUser(ctx, userID)
	if err != nil {
		return domain.Source{}, false, nil, err
	}
	var current *domain.Source
	for i, src := range own {
		if src.Kind != kindGrowatt {
			continue
		}
		if current == nil || src.CredentialEnc != "" {
			current = &own[i]
		}
	}

	switch {
	case unowned != nil:
		if err := m.st.ClaimSource(ctx, unowned.ID, userID); err != nil {
			return domain.Source{}, false, nil, err
		}
		return *unowned, true, current, nil
	case current != nil:
		return *current, false, nil, nil
	}

	cfg, err := json.Marshal(map[string]string{"base_url": m.baseURL})
	if err != nil {
		return domain.Source{}, false, nil, err
	}
	created, err := m.st.EnsureSource(ctx, domain.Source{
		Kind:       kindGrowatt,
		Name:       fmt.Sprintf("growatt-conta-%d", userID),
		ConfigJSON: string(cfg),
		Enabled:    true,
		UserID:     &userID,
	})
	if err != nil {
		return domain.Source{}, false, nil, err
	}
	return created, false, nil, nil
}

// prefetched answers one task with a result already in hand, through the normal
// runner, so the result is stored and the rate limit recorded like any other call.
type prefetched struct {
	connector.Connector
	key    string
	result connector.Result
	used   bool
}

func (p *prefetched) Execute(ctx context.Context, task connector.Task) (connector.Result, error) {
	if !p.used && task.LimitKey == p.key {
		p.used = true
		return p.result, nil
	}
	return p.Connector.Execute(ctx, task)
}

// seed stores a listing already fetched. If the queue cannot run that task now,
// the task stays queued and the collector fetches it later.
func (m *Manager) seed(ctx context.Context, src domain.Source, conn connector.Connector, task connector.Task, result connector.Result) error {
	if err := Discover(ctx, m.st, src.ID, conn, time.Now()); err != nil {
		return err
	}
	stub := &prefetched{Connector: conn, key: task.LimitKey, result: result}
	runner := NewRunner(m.st, src.ID, stub, WithLogger(m.logger))
	for range 3 {
		processed, err := runner.Step(ctx)
		if err != nil {
			return err
		}
		if stub.used || !processed {
			return nil
		}
	}
	return nil
}

// Disconnect removes the account's tokens and stops collecting. The data stays.
func (m *Manager) Disconnect(ctx context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	own, err := m.st.ListSourcesForUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, src := range own {
		if src.Kind != kindGrowatt {
			continue
		}
		m.stop(src.ID)
		if err := m.st.ClearSourceCredential(ctx, src.ID); err != nil {
			return err
		}
	}
	return nil
}

// start launches the runner and scheduler of a source. The caller holds m.mu.
func (m *Manager) start(src domain.Source, conn connector.Connector) {
	ctx, cancel := context.WithCancel(m.ctx)
	c := &collector{cancel: cancel, done: make(chan struct{})}
	m.collectors[src.ID] = c

	runner := NewRunner(m.st, src.ID, conn, WithLogger(m.logger))
	scheduler := NewScheduler(m.st, src.ID, conn, WithSchedulerLogger(m.logger))

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.logf("runner for source %q exited: %v", src.Name, err)
		}
	})
	wg.Go(func() {
		if err := scheduler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.logf("scheduler for source %q exited: %v", src.Name, err)
		}
	})
	go func() {
		wg.Wait()
		close(c.done)
	}()
}

// stop ends the collector of a source and waits for it, so a token change never
// leaves two collectors on one source. The caller holds m.mu.
func (m *Manager) stop(sourceID int64) {
	c, ok := m.collectors[sourceID]
	if !ok {
		return
	}
	c.cancel()
	<-c.done
	delete(m.collectors, sourceID)
}

// Collecting reports whether a source has a running collector.
func (m *Manager) Collecting(sourceID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.collectors[sourceID]
	return ok
}

// Wait stops every collector. Use it to shut down cleanly.
func (m *Manager) Wait() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.collectors {
		m.stop(id)
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m.logger != nil {
		m.logger.Printf(format, args...)
	}
}
