package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/id"
)

const OperatorScope = "operator"

const sortableTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

type APIToken struct {
	ID    string
	Name  string
	Scope string
}

type BrowserSession struct {
	TokenID   string
	TokenName string
	ExpiresAt time.Time
}

func (s *Store) CreateAPIToken(ctx context.Context, name, rawToken string) (APIToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return APIToken{}, errors.New("token name must contain between 1 and 100 characters")
	}
	if len(rawToken) < 32 {
		return APIToken{}, errors.New("token must contain at least 32 characters")
	}
	tokenID, err := id.New("tok")
	if err != nil {
		return APIToken{}, err
	}
	token := APIToken{ID: tokenID, Name: name, Scope: OperatorScope}
	hash := sha256.Sum256([]byte(rawToken))
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO api_tokens(id, name, token_hash, scope, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, token.ID, token.Name, hash[:], token.Scope, formatTime(s.now()))
	if err != nil {
		return APIToken{}, fmt.Errorf("create API token: %w", err)
	}
	return token, nil
}

func (s *Store) AuthenticateAPIToken(ctx context.Context, rawToken string) (APIToken, error) {
	hash := sha256.Sum256([]byte(rawToken))
	var token APIToken
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, scope
		FROM api_tokens
		WHERE token_hash = ? AND revoked_at IS NULL
	`, hash[:]).Scan(&token.ID, &token.Name, &token.Scope)
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, ErrNotFound
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("authenticate API token: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE api_tokens SET last_used_at = ? WHERE id = ?
	`, formatTime(s.now()), token.ID); err != nil {
		return APIToken{}, fmt.Errorf("record API token use: %w", err)
	}
	return token, nil
}

func (s *Store) CreateBrowserSession(ctx context.Context, tokenID, rawSession string, expiresAt time.Time) error {
	if tokenID == "" || len(rawSession) < 32 {
		return errors.New("token ID and a session secret of at least 32 characters are required")
	}
	now := s.now().UTC()
	if !expiresAt.After(now) {
		return errors.New("session expiry must be in the future")
	}
	hash := sha256.Sum256([]byte(rawSession))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin browser session: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM browser_sessions WHERE expires_at <= ?", formatSortableTime(now)); err != nil {
		return fmt.Errorf("remove expired browser sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO browser_sessions(session_hash, api_token_id, created_at, expires_at, last_used_at)
		VALUES (?, ?, ?, ?, ?)
	`, hash[:], tokenID, formatSortableTime(now), formatSortableTime(expiresAt), formatSortableTime(now)); err != nil {
		return fmt.Errorf("create browser session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit browser session: %w", err)
	}
	return nil
}

func (s *Store) AuthenticateBrowserSession(ctx context.Context, rawSession string) (BrowserSession, error) {
	hash := sha256.Sum256([]byte(rawSession))
	now := s.now().UTC()
	var session BrowserSession
	var expiresAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.name, s.expires_at
		FROM browser_sessions AS s
		JOIN api_tokens AS t ON t.id = s.api_token_id
		WHERE s.session_hash = ? AND s.expires_at > ? AND t.revoked_at IS NULL
	`, hash[:], formatSortableTime(now)).Scan(&session.TokenID, &session.TokenName, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BrowserSession{}, ErrNotFound
	}
	if err != nil {
		return BrowserSession{}, fmt.Errorf("authenticate browser session: %w", err)
	}
	session.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return BrowserSession{}, fmt.Errorf("parse browser session expiry: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE browser_sessions SET last_used_at = ? WHERE session_hash = ?
	`, formatSortableTime(now), hash[:]); err != nil {
		return BrowserSession{}, fmt.Errorf("record browser session use: %w", err)
	}
	return session, nil
}

func (s *Store) DeleteBrowserSession(ctx context.Context, rawSession string) error {
	hash := sha256.Sum256([]byte(rawSession))
	if _, err := s.db.ExecContext(ctx, "DELETE FROM browser_sessions WHERE session_hash = ?", hash[:]); err != nil {
		return fmt.Errorf("delete browser session: %w", err)
	}
	return nil
}

func formatSortableTime(value time.Time) string {
	return value.UTC().Format(sortableTimeFormat)
}
