package data

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"errors"
	"slices"
	"time"

	"github.com/lib/pq"
)

const (
	ScopeChat  = "chat"
	ScopeAdmin = "admin"
)

const (
	APIKeyPrefix     = "gw_"
	apiKeyRandBytes  = 32
	apiKeyDisplayLen = 10
	// "gw_" + base32 (no padding) of 32 bytes = 3 + 52 characters.
	apiKeyLength = 55
)

type APIKey struct {
	Plaintext       string     `json:"key,omitempty"`
	Hash            []byte     `json:"-"`
	ID              int64      `json:"id"`
	UserID          int64      `json:"-"`
	Name            string     `json:"name"`
	Prefix          string     `json:"prefix"`
	Scopes          []string   `json:"scopes"`
	RateLimitRPS    *float64   `json:"rate_limit_rps"`
	RateLimitBurst  *int       `json:"rate_limit_burst"`
	TokensPerMinute *int       `json:"tokens_per_minute"`
	CreatedAt       time.Time  `json:"created_at"`
	LastUsedAt      *time.Time `json:"last_used_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
	RevokedAt       *time.Time `json:"revoked_at"`
}

type APIKeyModel struct {
	DB *sql.DB
}

// HasScope reports whether the key was granted the given scope.
func (k *APIKey) HasScope(scope string) bool {
	return slices.Contains(k.Scopes, scope)
}

// hashAPIKey returns the SHA-256 hash stored in api_keys.hash.
func hashAPIKey(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}

// Generate a new API key
func generateAPIKey(userID int64, name string, scopes []string, ttl time.Duration) (*APIKey, error) {
	randomBytes := make([]byte, apiKeyRandBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return nil, err
	}

	plaintext := APIKeyPrefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomBytes)

	key := &APIKey{
		Plaintext: plaintext,
		Hash:      hashAPIKey(plaintext),
		UserID:    userID,
		Name:      name,
		Prefix:    plaintext[:apiKeyDisplayLen],
		Scopes:    scopes,
	}

	if ttl > 0 {
		expiry := time.Now().Add(ttl)
		key.ExpiresAt = &expiry
	}

	return key, nil
}

// Insert API key into the db for a given UserId
func (m APIKeyModel) Insert(key *APIKey) error {
	query := `
		INSERT INTO api_keys (user_id, name, key_prefix, hash, scopes,
		                      rate_limit_rps, rate_limit_burst, tokens_per_minute, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`

	args := []any{
		key.UserID,
		key.Name,
		key.Prefix,
		key.Hash,
		pq.Array(key.Scopes),
		key.RateLimitRPS,
		key.RateLimitBurst,
		key.TokensPerMinute,
		key.ExpiresAt,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return m.DB.QueryRowContext(ctx, query, args...).Scan(&key.ID, &key.CreatedAt)
}

// GetForPlaintext hashes the plaintext and returns the key and its user, but
// only if the key is not revoked, not expired, and the user is activated.
// Used by the authenticate middleware.
func (m APIKeyModel) GetForPlaintext(plaintext string) (*User, *APIKey, error) {
	query := `
		SELECT users.id, users.created_at, users.name, users.email, users.activated, users.version,
		       api_keys.id, api_keys.user_id, api_keys.name, api_keys.key_prefix, api_keys.scopes,
		       api_keys.rate_limit_rps, api_keys.rate_limit_burst, api_keys.tokens_per_minute,
		       api_keys.created_at, api_keys.last_used_at, api_keys.expires_at
		FROM api_keys
		INNER JOIN users ON users.id = api_keys.user_id
		WHERE api_keys.hash = $1
		  AND api_keys.revoked_at IS NULL
		  AND (api_keys.expires_at IS NULL OR api_keys.expires_at > now())
		  AND users.activated`

	var user User
	var key APIKey

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.DB.QueryRowContext(ctx, query, hashAPIKey(plaintext)).Scan(
		&user.ID,
		&user.CreatedAt,
		&user.Name,
		&user.Email,
		&user.Activated,
		&user.Version,
		&key.ID,
		&key.UserID,
		&key.Name,
		&key.Prefix,
		pq.Array(&key.Scopes),
		&key.RateLimitRPS,
		&key.RateLimitBurst,
		&key.TokensPerMinute,
		&key.CreatedAt,
		&key.LastUsedAt,
		&key.ExpiresAt,
	)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil, ErrRecordNotFound
		default:
			return nil, nil, err
		}
	}

	return &user, &key, nil
}

// GetAllForUser lists a user's keys, newest first (admin endpoint, Nov 18).
func (m APIKeyModel) GetAllForUser(userID int64) ([]*APIKey, error) {
	query := `
		SELECT id, user_id, name, key_prefix, scopes,
		       rate_limit_rps, rate_limit_burst, tokens_per_minute,
		       created_at, last_used_at, expires_at, revoked_at
		FROM api_keys
		WHERE user_id = $1
		ORDER BY created_at DESC`

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := m.DB.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []*APIKey{}
	for rows.Next() {
		var key APIKey
		err := rows.Scan(
			&key.ID,
			&key.UserID,
			&key.Name,
			&key.Prefix,
			pq.Array(&key.Scopes),
			&key.RateLimitRPS,
			&key.RateLimitBurst,
			&key.TokensPerMinute,
			&key.CreatedAt,
			&key.LastUsedAt,
			&key.ExpiresAt,
			&key.RevokedAt,
		)
		if err != nil {
			return nil, err
		}
		keys = append(keys, &key)
	}

	return keys, rows.Err()
}

// Touch records that a key was used. The WHERE clause throttles it to one
// write per minute per key, so the middleware can call it on every request.
func (m APIKeyModel) Touch(id int64) error {
	query := `
		UPDATE api_keys
		SET last_used_at = now()
		WHERE id = $1
		  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := m.DB.ExecContext(ctx, query, id)
	return err
}

// Revoke marks a key as revoked. Returns ErrRecordNotFound if the key does
// not exist, belongs to another user, or is already revoked.
func (m APIKeyModel) Revoke(id, userID int64) error {
	query := `
		UPDATE api_keys
		SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := m.DB.ExecContext(ctx, query, id, userID)
	if err != nil {
		return err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrRecordNotFound
	}

	return nil
}
