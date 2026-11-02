package data

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"math/big"
	"time"

	"github.com/lib/pq"

	"gateway.jackwillis517.net/internal/validator"
)

const (
	// LGF Ch. 15 scopes, plus this gateway's own scopes for later endpoints.
	ScopeAuthentication = "authentication"
	ScopeActivation     = "activation"
	ScopeChat           = "chat"
	ScopeAdmin          = "admin"
)

// A stateful authentication token: the plaintext is shown once, only its
// SHA-256 hash is stored (LGF Ch. 15).
type Token struct {
	Plaintext       string     `json:"token,omitempty"`
	Hash            []byte     `json:"-"`
	ID              int64      `json:"id"`
	UserID          int64      `json:"-"`
	Name            string     `json:"name"`
	Scopes          []string   `json:"scopes"`
	RateLimitRPS    *float64   `json:"rate_limit_rps"`
	RateLimitBurst  *int       `json:"rate_limit_burst"`
	TokensPerMinute *int       `json:"tokens_per_minute"`
	CreatedAt       time.Time  `json:"created_at"`
	LastUsedAt      *time.Time `json:"last_used_at"`
	ExpiresAt       *time.Time `json:"expires_at"`
	RevokedAt       *time.Time `json:"revoked_at"`
}

type TokenModel struct {
	DB *sql.DB
}

// hashToken returns the SHA-256 hash stored in tokens.hashed_token.
func hashToken(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}

// tokenLength is LGF's 26-character token; tokenRandBytes of crypto/rand
// entropy is base62-encoded into it.
const (
	tokenLength    = 26
	tokenRandBytes = 16
)

// generateToken creates a new Token with a 26-character base62 plaintext
// (LGF's generateToken, adapted for the spec's name column).
func generateToken(userID int64, name string, scopes []string, ttl time.Duration) (*Token, error) {
	randomBytes := make([]byte, tokenRandBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return nil, err
	}

	plaintext, err := base62Encode(randomBytes)
	if err != nil {
		return nil, err
	}
	// Left-pad with 'A' so every token is exactly 26 characters, regardless
	// of how many leading zero bytes the random source produced.
	for len(plaintext) < tokenLength {
		plaintext = "A" + plaintext
	}

	token := &Token{
		Plaintext: plaintext,
		Hash:      hashToken(plaintext),
		UserID:    userID,
		Name:      name,
		Scopes:    scopes,
	}

	if ttl > 0 {
		expiry := time.Now().Add(ttl)
		token.ExpiresAt = &expiry
	}

	return token, nil
}

// base62Encode encodes bytes as a big-endian base62 string.
func base62Encode(b []byte) (string, error) {
	var alphabet = []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")

	n := new(big.Int).SetBytes(b)
	base := big.NewInt(62)
	zero := big.NewInt(0)
	mod := new(big.Int)

	out := []byte{}
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		out = append([]byte{alphabet[mod.Int64()]}, out...)
	}

	return string(out), nil
}

// New generates and stores a token for the user (LGF 15.4, adapted: the
// spec's tokens table requires a name).
func (m TokenModel) New(userID int64, name string, ttl time.Duration, scopes ...string) (*Token, error) {
	token, err := generateToken(userID, name, scopes, ttl)
	if err != nil {
		return nil, err
	}
	err = m.Insert(token)
	return token, err
}

// Insert stores the token's hash, never the plaintext.
func (m TokenModel) Insert(token *Token) error {
	query := `
		INSERT INTO tokens (user_id, name, hashed_token, scopes,
		                    rate_limit_rps, rate_limit_burst, tokens_per_minute, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at`

	args := []any{
		token.UserID,
		token.Name,
		token.Hash,
		pq.Array(token.Scopes),
		token.RateLimitRPS,
		token.RateLimitBurst,
		token.TokensPerMinute,
		token.ExpiresAt,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return m.DB.QueryRowContext(ctx, query, args...).Scan(&token.ID, &token.CreatedAt)
}

// GetForPlaintext hashes the plaintext and returns the token and its user,
// but only if the token is not revoked, not expired, and the user is
// activated. Used by the authenticate middleware.
func (m TokenModel) GetForPlaintext(plaintext string) (*User, *Token, error) {
	query := `
		SELECT users.id, users.created_at, users.name, users.email, users.activated, users.version,
		       tokens.id, tokens.user_id, tokens.name, tokens.scopes,
		       tokens.rate_limit_rps, tokens.rate_limit_burst, tokens.tokens_per_minute,
		       tokens.created_at, tokens.last_used_at, tokens.expires_at
		FROM tokens
		INNER JOIN users ON users.id = tokens.user_id
		WHERE tokens.hashed_token = $1
		  AND tokens.revoked_at IS NULL
		  AND (tokens.expires_at IS NULL OR tokens.expires_at > now())
		  AND users.activated`

	var user User
	var token Token

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.DB.QueryRowContext(ctx, query, hashToken(plaintext)).Scan(
		&user.ID,
		&user.CreatedAt,
		&user.Name,
		&user.Email,
		&user.Activated,
		&user.Version,
		&token.ID,
		&token.UserID,
		&token.Name,
		pq.Array(&token.Scopes),
		&token.RateLimitRPS,
		&token.RateLimitBurst,
		&token.TokensPerMinute,
		&token.CreatedAt,
		&token.LastUsedAt,
		&token.ExpiresAt,
	)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil, ErrRecordNotFound
		default:
			return nil, nil, err
		}
	}

	return &user, &token, nil
}

// Touch records that a token was used. The WHERE clause throttles it to one
// write per minute per token, so the middleware can call it on every request.
func (m TokenModel) Touch(id int64) error {
	query := `
		UPDATE tokens
		SET last_used_at = now()
		WHERE id = $1
		  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := m.DB.ExecContext(ctx, query, id)
	return err
}

// Revoke marks a token as revoked. Returns ErrRecordNotFound if the token
// does not exist, belongs to another user, or is already revoked.
func (m TokenModel) Revoke(id, userID int64) error {
	query := `
		UPDATE tokens
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

// ValidateTokenPlaintext checks that a token is present and the right shape
// (LGF 15.4).
func ValidateTokenPlaintext(v *validator.Validator, tokenPlaintext string) {
	v.Check(tokenPlaintext != "", "token", "must be provided")
	v.Check(len(tokenPlaintext) == 26, "token", "must be 26 bytes long")
}
