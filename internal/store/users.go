package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luigiazoreng/solarguard/internal/domain"
)

// ErrEmailTaken reports that an account already uses the e-mail.
var ErrEmailTaken = errors.New("e-mail já cadastrado")

// ownedPlantIDs selects the plants of one account. A plant belongs to whoever
// owns its source, so a source without an owner hides its plants from everyone.
const ownedPlantIDs = `(SELECT p.id FROM plants p JOIN sources src ON src.id = p.source_id WHERE src.user_id = ?)`

// User is one panel account.
type User struct {
	ID           int64
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

// CreateUser stores a new account. The e-mail must already be normalized.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (User, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO users (email, password_hash, created_at) VALUES (?, ?, ?)
		ON CONFLICT (email) DO NOTHING`, email, passwordHash, formatTime(s.now()))
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	if inserted == 0 {
		return User{}, ErrEmailTaken
	}
	return s.GetUserByEmail(ctx, email)
}

// GetUserByEmail returns one account or ErrNotFound.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	var createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT id, email, password_hash, created_at FROM users WHERE email = ?`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// SetUserPassword replaces the password hash of an account, or returns ErrNotFound.
func (s *Store) SetUserPassword(ctx context.Context, email, passwordHash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE email = ?`, passwordHash, email)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	updated, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if updated == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPlantsForUser returns the plants of one account, ordered by name.
func (s *Store) ListPlantsForUser(ctx context.Context, userID int64) ([]domain.Plant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, source_id, external_id, COALESCE(name_override, name) AS name, timezone, latitude, longitude, peak_power_kw, weather_location_id
		FROM plants WHERE id IN `+ownedPlantIDs+` ORDER BY name, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list plants for user: %w", err)
	}
	defer rows.Close()
	var out []domain.Plant
	for rows.Next() {
		var p domain.Plant
		if err := rows.Scan(&p.ID, &p.SourceID, &p.ExternalID, &p.Name, &p.Timezone, &p.Latitude, &p.Longitude, &p.PeakPowerKW, &p.WeatherLocationID); err != nil {
			return nil, fmt.Errorf("scan plant: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PlantOwnedBy reports whether the plant belongs to the account.
func (s *Store) PlantOwnedBy(ctx context.Context, plantID, userID int64) (bool, error) {
	var owned bool
	err := s.db.QueryRowContext(ctx, `SELECT ? IN `+ownedPlantIDs, plantID, userID).Scan(&owned)
	if err != nil {
		return false, fmt.Errorf("check plant owner: %w", err)
	}
	return owned, nil
}

// ListSourcesForUser returns the sources of one account, oldest first.
func (s *Store) ListSourcesForUser(ctx context.Context, userID int64) ([]domain.Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM sources WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sources for user: %w", err)
	}
	return collectSources(rows)
}

// SourcesWithPlants returns the sources of a kind that hold any of the given
// provider plant IDs.
func (s *Store) SourcesWithPlants(ctx context.Context, kind string, externalIDs []string) ([]domain.Source, error) {
	if len(externalIDs) == 0 {
		return nil, nil
	}
	args := []any{kind}
	for _, id := range externalIDs {
		args = append(args, id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(externalIDs)), ",")
	rows, err := s.db.QueryContext(ctx, `SELECT `+sourceColumns+` FROM sources WHERE kind = ? AND id IN
		(SELECT source_id FROM plants WHERE external_id IN (`+placeholders+`)) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("find sources by plants: %w", err)
	}
	return collectSources(rows)
}

// ClaimSource gives a source without an owner to the account. It returns
// ErrNotFound when the source does not exist or already has an owner.
func (s *Store) ClaimSource(ctx context.Context, sourceID, userID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sources SET user_id = ? WHERE id = ? AND user_id IS NULL`, userID, sourceID)
	if err != nil {
		return fmt.Errorf("claim source: %w", err)
	}
	claimed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("claim source: %w", err)
	}
	if claimed == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSourceCredential stores an encrypted token. It also drops the environment
// variable name, so the source no longer depends on the environment.
func (s *Store) SetSourceCredential(ctx context.Context, sourceID int64, encrypted string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sources SET credential_enc = ?, credential_env = '' WHERE id = ?`, encrypted, sourceID)
	if err != nil {
		return fmt.Errorf("set source credential: %w", err)
	}
	return nil
}

// ClearSourceCredential removes the token of a source. Its data stays.
func (s *Store) ClearSourceCredential(ctx context.Context, sourceID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sources SET credential_enc = '', credential_env = '' WHERE id = ?`, sourceID)
	if err != nil {
		return fmt.Errorf("clear source credential: %w", err)
	}
	return nil
}
