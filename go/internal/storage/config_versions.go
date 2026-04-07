package storage

import (
	"database/sql"
	"errors"
	"fmt"

	"hazuki-go/internal/model"
)

type ConfigVersion struct {
	ID        int64
	CreatedAt string
	CreatedBy string
	Note      string
}

func (s *ConfigStore) ListVersions(limit int) ([]ConfigVersion, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	s.mu.RLock()
	inited := s.inited
	s.mu.RUnlock()
	if !inited {
		return nil, errors.New("ConfigStore not initialized")
	}

	rows, err := s.db.Query(`
SELECT
  v.id AS id,
  v.created_at AS created_at,
  COALESCE(u.username, '') AS created_by,
  COALESCE(v.note, '') AS note
FROM config_versions v
LEFT JOIN users u ON u.id = v.created_by
ORDER BY v.id DESC
LIMIT ?
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ConfigVersion, 0, limit)
	for rows.Next() {
		var v ConfigVersion
		if err := rows.Scan(&v.ID, &v.CreatedAt, &v.CreatedBy, &v.Note); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *ConfigStore) RestoreVersion(versionID int64, userID *int64) error {
	if versionID <= 0 {
		return errors.New("invalid version id")
	}

	s.mu.Lock()
	if !s.inited {
		s.mu.Unlock()
		return errors.New("ConfigStore not initialized")
	}

	row := s.db.QueryRow("SELECT config_json FROM config_versions WHERE id = ?", versionID)
	var configJSON string
	if err := row.Scan(&configJSON); err != nil {
		s.mu.Unlock()
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("Config version not found")
		}
		return err
	}

	encrypted, err := decodeConfigRow(configJSON, s.crypto)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("version config json: %w", err)
	}
	if err := encrypted.Validate(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("version config invalid: %w", err)
	}

	encoded, err := encodeConfigRow(encrypted, s.crypto)
	if err != nil {
		s.mu.Unlock()
		return err
	}

	now := nowIso()
	tx, err := s.db.Begin()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		"UPDATE config_current SET config_json = ?, updated_at = ?, updated_by = ? WHERE id = ?",
		encoded,
		now,
		userID,
		configRowID,
	); err != nil {
		s.mu.Unlock()
		return err
	}

	if _, err := tx.Exec(
		"INSERT INTO config_versions (config_json, created_at, created_by, note) VALUES (?, ?, ?, ?)",
		encoded,
		now,
		userID,
		fmt.Sprintf("restore:%d", versionID),
	); err != nil {
		s.mu.Unlock()
		return err
	}

	if err := tx.Commit(); err != nil {
		s.mu.Unlock()
		return err
	}

	decrypted, err := decryptConfigSecrets(encrypted, s.crypto)
	if err != nil {
		s.mu.Unlock()
		return err
	}

	s.encrypted = encrypted
	s.decrypted = decrypted
	s.updatedAt = now

	listeners := append([]func(model.AppConfig){}, s.onChanged...)
	decryptedClone, _ := cloneConfig(decrypted)
	s.mu.Unlock()

	for _, cb := range listeners {
		if cb == nil {
			continue
		}
		cb(decryptedClone)
	}

	return nil
}
