package data

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Password  password  `json:"-"`
	Activated bool      `json:"activated"`
	Version   int       `json:"-"`
}

// password is an unexported struct type that holds the plaintext password
// and its bcrypt hash (LGF Ch. 9).
type password struct {
	plaintext *string
	hash      []byte
}

// Set calculates the bcrypt hash of a plaintext password and stores both.
func (p *password) Set(plaintextPassword string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintextPassword), 12)
	if err != nil {
		return err
	}
	p.plaintext = &plaintextPassword
	p.hash = hash
	return nil
}

// Matches compares a plaintext password with the stored hash. It returns
// false (not an error) when the password does not match (LGF Ch. 9).
func (p *password) Matches(plaintextPassword string) (bool, error) {
	if p.hash == nil {
		return false, nil
	}
	err := bcrypt.CompareHashAndPassword(p.hash, []byte(plaintextPassword))
	if err != nil {
		switch {
		case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
			return false, nil
		default:
			return false, err
		}
	}
	return true, nil
}

type UserModel struct {
	DB *sql.DB
}

var ErrDuplicateEmail = errors.New("duplicate email")

var AnonymousUser = &User{
	ID: 1,
}

func (u *User) IsAnonymous() bool {
	return u == AnonymousUser
}

func (m UserModel) GetByEmail(email string) (*User, error) {
	query := `
SELECT id, created_at, name, email, password_hash, activated, version
FROM users
WHERE email = $1`

	var user User

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.DB.QueryRowContext(ctx, query, email).Scan(
		&user.ID,
		&user.CreatedAt,
		&user.Name,
		&user.Email,
		&user.Password.hash,
		&user.Activated,
		&user.Version,
	)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, ErrRecordNotFound
		default:
			return nil, err
		}
	}

	return &user, nil
}
