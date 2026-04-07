package storage

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

func CreateSession(db *sql.DB, userID int64, ttlSeconds int) (string, error) {
	token, err := generateToken()
	if err != nil {
		return "", err
	}

	tokenHash := sha256Hex(token)
	createdAt := nowIso()
	expiresAt := time.Now().Add(time.Duration(ttlSeconds) * time.Second).UTC().Format(time.RFC3339Nano)

	_, err = db.Exec(
		"INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)",
		tokenHash,
		userID,
		createdAt,
		expiresAt,
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

func DeleteSession(db *sql.DB, token string) error {
	tokenHash := sha256Hex(strings.TrimSpace(token))
	_, err := db.Exec("DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	return err
}

func GetSessionUser(db *sql.DB, token string) (User, bool, error) {
	tokenHash := sha256Hex(strings.TrimSpace(token))

	var userID int64
	var expiresAt string
	var username string

	err := db.QueryRow(`
SELECT
  s.user_id AS user_id,
  s.expires_at AS expires_at,
  u.username AS username
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ?
`, tokenHash).Scan(&userID, &expiresAt, &username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, false, nil
		}
		return User{}, false, err
	}

	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		// Best-effort: treat invalid as expired.
		return User{}, false, nil
	}
	if !exp.After(time.Now().UTC()) {
		return User{}, false, nil
	}

	return User{ID: userID, Username: username}, true, nil
}

func DeleteSessionsByUser(db *sql.DB, userID int64) error {
	_, err := db.Exec("DELETE FROM sessions WHERE user_id = ?", userID)
	return err
}

func CleanupExpiredSessions(db *sql.DB) error {
	now := nowIso()
	_, err := db.Exec("DELETE FROM sessions WHERE expires_at <= ?", now)
	return err
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
