package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"hazuki-go/internal/model"
)

const configRowID = 1

type ConfigStore struct {
	db     *sql.DB
	crypto *CryptoContext

	mu        sync.RWMutex
	encrypted model.AppConfig
	decrypted model.AppConfig
	updatedAt string
	inited    bool

	onChanged []func(model.AppConfig)
}

func NewConfigStore(db *sql.DB, crypto *CryptoContext) *ConfigStore {
	return &ConfigStore{db: db, crypto: crypto}
}

func (s *ConfigStore) IsEncryptionEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.crypto != nil && s.crypto.Enabled
}

func (s *ConfigStore) InitFromEnvironment(getEnv func(string) string, lookupEnv func(string) (string, bool)) error {
	row := s.db.QueryRow("SELECT config_json, updated_at FROM config_current WHERE id = ?", configRowID)

	var configJSON string
	var updatedAt string
	err := row.Scan(&configJSON, &updatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if err == nil && configJSON != "" {
		encrypted, err := decodeConfigRow(configJSON, s.crypto)
		if err != nil {
			return err
		}

		decrypted, err := decryptConfigSecrets(encrypted, s.crypto)
		if err != nil {
			return err
		}
		if err := decrypted.Validate(); err != nil {
			return err
		}

		if upgraded, ok, err := s.maybeUpgradeConfigEncryption(configJSON, decrypted, s.crypto); err != nil {
			return err
		} else if ok {
			encrypted = upgraded
		}

		s.mu.Lock()
		s.encrypted = encrypted
		s.decrypted = decrypted
		s.updatedAt = updatedAt
		s.inited = true
		s.mu.Unlock()
		return nil
	}

	seed, err := model.DefaultConfigFromEnv(getEnv, lookupEnv)
	if err != nil {
		return err
	}

	encrypted, err := encryptConfigSecrets(seed, s.crypto)
	if err != nil {
		return err
	}

	encoded, err := encodeConfigRow(encrypted, s.crypto)
	if err != nil {
		return err
	}

	now := nowIso()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		"INSERT INTO config_current (id, config_json, updated_at, updated_by) VALUES (?, ?, ?, ?)",
		configRowID,
		encoded,
		now,
		nil,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(
		"INSERT INTO config_versions (config_json, created_at, created_by, note) VALUES (?, ?, ?, ?)",
		encoded,
		now,
		nil,
		"seed",
	); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	s.mu.Lock()
	s.encrypted = encrypted
	s.decrypted = seed
	s.updatedAt = now
	s.inited = true
	s.mu.Unlock()

	return nil
}

func (s *ConfigStore) ReloadFromDB(crypto *CryptoContext) error {
	if s == nil || s.db == nil {
		return errors.New("ConfigStore is nil")
	}

	row := s.db.QueryRow("SELECT config_json, updated_at FROM config_current WHERE id = ?", configRowID)

	var configJSON string
	var updatedAt string
	if err := row.Scan(&configJSON, &updatedAt); err != nil {
		return err
	}
	if strings.TrimSpace(configJSON) == "" {
		return errors.New("config_current is empty")
	}

	useCrypto := crypto
	if useCrypto == nil {
		useCrypto = s.crypto
	}

	encrypted, err := decodeConfigRow(configJSON, useCrypto)
	if err != nil {
		return err
	}

	decrypted, err := decryptConfigSecrets(encrypted, useCrypto)
	if err != nil {
		return err
	}
	if err := decrypted.Validate(); err != nil {
		return err
	}

	if upgraded, ok, err := s.maybeUpgradeConfigEncryption(configJSON, decrypted, useCrypto); err != nil {
		return err
	} else if ok {
		encrypted = upgraded
	}

	s.mu.Lock()
	s.crypto = useCrypto
	s.encrypted = encrypted
	s.decrypted = decrypted
	s.updatedAt = updatedAt
	s.inited = true
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

func (s *ConfigStore) GetUpdatedAt() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updatedAt
}

func (s *ConfigStore) GetEncryptedConfig() (model.AppConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.inited {
		return model.AppConfig{}, errors.New("ConfigStore not initialized")
	}
	return cloneConfig(s.encrypted)
}

func (s *ConfigStore) GetDecryptedConfig() (model.AppConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.inited {
		return model.AppConfig{}, errors.New("ConfigStore not initialized")
	}
	return cloneConfig(s.decrypted)
}

func (s *ConfigStore) GetRedactedConfig() (model.AppConfig, error) {
	cfg, err := s.GetDecryptedConfig()
	if err != nil {
		return model.AppConfig{}, err
	}
	if cfg.Git.GithubToken != "" {
		cfg.Git.GithubToken = "__SET__"
	}
	for i := range cfg.GitInstances {
		if cfg.GitInstances[i].Git.GithubToken != "" {
			cfg.GitInstances[i].Git.GithubToken = "__SET__"
		}
	}
	if cfg.Torcherino.WorkerSecretKey != "" {
		cfg.Torcherino.WorkerSecretKey = "__SET__"
	}
	if cfg.Sakuya.Oplist.Token != "" {
		cfg.Sakuya.Oplist.Token = "__SET__"
	}
	for i := range cfg.Sakuya.Instances {
		if cfg.Sakuya.Instances[i].Token != "" {
			cfg.Sakuya.Instances[i].Token = "__SET__"
		}
	}
	if cfg.Patchouli.Token != "" {
		cfg.Patchouli.Token = "__SET__"
	}
	if cfg.Patchouli.AccessKey != "" {
		cfg.Patchouli.AccessKey = "__SET__"
	}
	if cfg.Torcherino.WorkerSecretHeaderMap != nil {
		out := make(map[string]string, len(cfg.Torcherino.WorkerSecretHeaderMap))
		for k, v := range cfg.Torcherino.WorkerSecretHeaderMap {
			if v != "" {
				out[k] = "__SET__"
			} else {
				out[k] = ""
			}
		}
		cfg.Torcherino.WorkerSecretHeaderMap = out
	}
	return cfg, nil
}

type UpdateRequest struct {
	Updater func(model.AppConfig) (model.AppConfig, error)
	UserID  *int64
	Note    string

	PreserveEmptySecrets bool
	ClearSecrets         []string
}

func (s *ConfigStore) Update(req UpdateRequest) error {
	if req.Updater == nil {
		return errors.New("updater is required")
	}

	s.mu.Lock()
	if !s.inited {
		s.mu.Unlock()
		return errors.New("ConfigStore not initialized")
	}

	current := s.decrypted
	currentClone, err := cloneConfig(current)
	if err != nil {
		s.mu.Unlock()
		return err
	}

	next, err := req.Updater(currentClone)
	if err != nil {
		s.mu.Unlock()
		return err
	}

	if req.PreserveEmptySecrets {
		clearSet := make(map[string]struct{})
		for _, p := range req.ClearSecrets {
			clearSet[p] = struct{}{}
		}
		if _, ok := clearSet["git.githubToken"]; !ok && next.Git.GithubToken == "" {
			next.Git.GithubToken = current.Git.GithubToken
		}

		if len(next.GitInstances) > 0 {
			curTokens := make(map[string]string, len(current.GitInstances))
			for _, inst := range current.GitInstances {
				id := strings.TrimSpace(inst.ID)
				if id == "" {
					continue
				}
				curTokens[strings.ToLower(id)] = inst.Git.GithubToken
			}

			for i := range next.GitInstances {
				id := strings.TrimSpace(next.GitInstances[i].ID)
				if id == "" {
					continue
				}
				path := "gitInstances." + id + ".git.githubToken"
				if _, ok := clearSet[path]; ok {
					continue
				}
				if next.GitInstances[i].Git.GithubToken != "" {
					continue
				}
				if tok, ok := curTokens[strings.ToLower(id)]; ok && tok != "" {
					next.GitInstances[i].Git.GithubToken = tok
				}
			}
		}

		if _, ok := clearSet["torcherino.workerSecretKey"]; !ok && next.Torcherino.WorkerSecretKey == "" {
			next.Torcherino.WorkerSecretKey = current.Torcherino.WorkerSecretKey
		}
		if _, ok := clearSet["torcherino.workerSecretHeaderMap"]; !ok && len(next.Torcherino.WorkerSecretHeaderMap) == 0 {
			if len(current.Torcherino.WorkerSecretHeaderMap) > 0 {
				copied := make(map[string]string, len(current.Torcherino.WorkerSecretHeaderMap))
				for k, v := range current.Torcherino.WorkerSecretHeaderMap {
					copied[k] = v
				}
				next.Torcherino.WorkerSecretHeaderMap = copied
			}
		}

		if _, ok := clearSet["sakuya.oplist.token"]; !ok && next.Sakuya.Oplist.Token == "" {
			next.Sakuya.Oplist.Token = current.Sakuya.Oplist.Token
		}

		if len(next.Sakuya.Instances) > 0 {
			curTokens := make(map[string]string, len(current.Sakuya.Instances))
			for _, inst := range current.Sakuya.Instances {
				id := strings.TrimSpace(inst.ID)
				if id == "" {
					continue
				}
				curTokens[strings.ToLower(id)] = inst.Token
			}

			for i := range next.Sakuya.Instances {
				id := strings.TrimSpace(next.Sakuya.Instances[i].ID)
				if id == "" {
					continue
				}
				path := "sakuya.instances." + id + ".token"
				if _, ok := clearSet[path]; ok {
					continue
				}
				if next.Sakuya.Instances[i].Token != "" {
					continue
				}
				if tok, ok := curTokens[strings.ToLower(id)]; ok && tok != "" {
					next.Sakuya.Instances[i].Token = tok
				}
			}
		}

		if _, ok := clearSet["patchouli.token"]; !ok && next.Patchouli.Token == "" {
			next.Patchouli.Token = current.Patchouli.Token
		}
		if _, ok := clearSet["patchouli.accessKey"]; !ok && next.Patchouli.AccessKey == "" {
			next.Patchouli.AccessKey = current.Patchouli.AccessKey
		}
	}

	if err := next.Validate(); err != nil {
		s.mu.Unlock()
		return err
	}

	encrypted, err := encryptConfigSecrets(next, s.crypto)
	if err != nil {
		s.mu.Unlock()
		return err
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
	defer func() { _ = tx.Rollback() }() // no-op after successful Commit

	if _, err := tx.Exec(
		"UPDATE config_current SET config_json = ?, updated_at = ?, updated_by = ? WHERE id = ?",
		encoded,
		now,
		req.UserID,
		configRowID,
	); err != nil {
		s.mu.Unlock()
		return err
	}

	if _, err := tx.Exec(
		"INSERT INTO config_versions (config_json, created_at, created_by, note) VALUES (?, ?, ?, ?)",
		encoded,
		now,
		req.UserID,
		nullIfEmpty(req.Note),
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

func (s *ConfigStore) OnChanged(fn func(model.AppConfig)) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChanged = append(s.onChanged, fn)
}

func encryptConfigSecrets(cfg model.AppConfig, crypto *CryptoContext) (model.AppConfig, error) {
	out, err := cloneConfig(cfg)
	if err != nil {
		return model.AppConfig{}, err
	}

	if crypto != nil {
		if out.Git.GithubToken != "" {
			enc, err := crypto.EncryptString(out.Git.GithubToken)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Git.GithubToken = enc
		}

		for i := range out.GitInstances {
			if out.GitInstances[i].Git.GithubToken == "" {
				continue
			}
			enc, err := crypto.EncryptString(out.GitInstances[i].Git.GithubToken)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.GitInstances[i].Git.GithubToken = enc
		}

		if out.Torcherino.WorkerSecretKey != "" {
			enc, err := crypto.EncryptString(out.Torcherino.WorkerSecretKey)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Torcherino.WorkerSecretKey = enc
		}

		if out.Sakuya.Oplist.Token != "" {
			enc, err := crypto.EncryptString(out.Sakuya.Oplist.Token)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Sakuya.Oplist.Token = enc
		}

		for i := range out.Sakuya.Instances {
			if out.Sakuya.Instances[i].Token == "" {
				continue
			}
			enc, err := crypto.EncryptString(out.Sakuya.Instances[i].Token)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Sakuya.Instances[i].Token = enc
		}

		if out.Patchouli.Token != "" {
			enc, err := crypto.EncryptString(out.Patchouli.Token)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Patchouli.Token = enc
		}

		if out.Patchouli.AccessKey != "" {
			enc, err := crypto.EncryptString(out.Patchouli.AccessKey)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Patchouli.AccessKey = enc
		}

		if out.Torcherino.WorkerSecretHeaderMap != nil {
			next := make(map[string]string, len(out.Torcherino.WorkerSecretHeaderMap))
			for k, v := range out.Torcherino.WorkerSecretHeaderMap {
				if v == "" {
					next[k] = ""
					continue
				}
				enc, err := crypto.EncryptString(v)
				if err != nil {
					return model.AppConfig{}, err
				}
				next[k] = enc
			}
			out.Torcherino.WorkerSecretHeaderMap = next
		}
	}

	return out, nil
}

func decryptConfigSecrets(cfg model.AppConfig, crypto *CryptoContext) (model.AppConfig, error) {
	out, err := cloneConfig(cfg)
	if err != nil {
		return model.AppConfig{}, err
	}

	if crypto != nil {
		if out.Git.GithubToken != "" {
			dec, err := crypto.DecryptString(out.Git.GithubToken)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Git.GithubToken = dec
		}

		for i := range out.GitInstances {
			if out.GitInstances[i].Git.GithubToken == "" {
				continue
			}
			dec, err := crypto.DecryptString(out.GitInstances[i].Git.GithubToken)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.GitInstances[i].Git.GithubToken = dec
		}

		if out.Torcherino.WorkerSecretKey != "" {
			dec, err := crypto.DecryptString(out.Torcherino.WorkerSecretKey)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Torcherino.WorkerSecretKey = dec
		}

		if out.Sakuya.Oplist.Token != "" {
			dec, err := crypto.DecryptString(out.Sakuya.Oplist.Token)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Sakuya.Oplist.Token = dec
		}

		for i := range out.Sakuya.Instances {
			if out.Sakuya.Instances[i].Token == "" {
				continue
			}
			dec, err := crypto.DecryptString(out.Sakuya.Instances[i].Token)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Sakuya.Instances[i].Token = dec
		}

		if out.Patchouli.Token != "" {
			dec, err := crypto.DecryptString(out.Patchouli.Token)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Patchouli.Token = dec
		}

		if out.Patchouli.AccessKey != "" {
			dec, err := crypto.DecryptString(out.Patchouli.AccessKey)
			if err != nil {
				return model.AppConfig{}, err
			}
			out.Patchouli.AccessKey = dec
		}

		if out.Torcherino.WorkerSecretHeaderMap != nil {
			next := make(map[string]string, len(out.Torcherino.WorkerSecretHeaderMap))
			for k, v := range out.Torcherino.WorkerSecretHeaderMap {
				if v == "" {
					next[k] = ""
					continue
				}
				dec, err := crypto.DecryptString(v)
				if err != nil {
					return model.AppConfig{}, err
				}
				next[k] = dec
			}
			out.Torcherino.WorkerSecretHeaderMap = next
		}
	}

	return out, nil
}

func cloneConfig(cfg model.AppConfig) (model.AppConfig, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return model.AppConfig{}, err
	}
	var out model.AppConfig
	if err := json.Unmarshal(b, &out); err != nil {
		return model.AppConfig{}, err
	}
	return out, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *ConfigStore) DebugString() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("ConfigStore{inited=%v, updatedAt=%q}", s.inited, s.updatedAt)
}

func (s *ConfigStore) maybeUpgradeConfigEncryption(storedConfigJSON string, decrypted model.AppConfig, crypto *CryptoContext) (model.AppConfig, bool, error) {
	if s == nil || s.db == nil || crypto == nil || !crypto.Enabled {
		return model.AppConfig{}, false, nil
	}
	if strings.HasPrefix(strings.TrimSpace(storedConfigJSON), encPrefix) {
		return model.AppConfig{}, false, nil
	}

	upgraded, err := encryptConfigSecrets(decrypted, crypto)
	if err != nil {
		return model.AppConfig{}, false, err
	}
	encoded, err := encodeConfigRow(upgraded, crypto)
	if err != nil {
		return model.AppConfig{}, false, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return model.AppConfig{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("UPDATE config_current SET config_json = ? WHERE id = ?", encoded, configRowID); err != nil {
		return model.AppConfig{}, false, err
	}

	rows, err := tx.Query("SELECT id, config_json FROM config_versions")
	if err != nil {
		return model.AppConfig{}, false, err
	}

	for rows.Next() {
		var id int64
		var cfg string
		if err := rows.Scan(&id, &cfg); err != nil {
			_ = rows.Close()
			return model.AppConfig{}, false, err
		}
		if strings.HasPrefix(strings.TrimSpace(cfg), encPrefix) {
			continue
		}
		encCfg, err := decodeConfigRow(cfg, crypto)
		if err != nil {
			continue
		}
		decCfg, err := decryptConfigSecrets(encCfg, crypto)
		if err != nil {
			continue
		}
		if err := decCfg.Validate(); err != nil {
			continue
		}
		nextEnc, err := encryptConfigSecrets(decCfg, crypto)
		if err != nil {
			continue
		}
		nextJSON, err := encodeConfigRow(nextEnc, crypto)
		if err != nil {
			continue
		}
		if _, err := tx.Exec("UPDATE config_versions SET config_json = ? WHERE id = ?", nextJSON, id); err != nil {
			continue
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return model.AppConfig{}, false, err
	}
	_ = rows.Close()

	if err := tx.Commit(); err != nil {
		return model.AppConfig{}, false, err
	}
	return upgraded, true, nil
}
