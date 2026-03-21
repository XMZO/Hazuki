package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const metaKeyRewriteAutoTuneActiveModel = "rewrite_autotune_active_model"

type RewriteAutoTuneActiveModel struct {
	AdmissionIntercept float64   `json:"admissionIntercept"`
	AdmissionSlope     float64   `json:"admissionSlope"`
	PromotedAt         time.Time `json:"promotedAt"`
}

func GetRewriteAutoTuneActiveModel(ctx context.Context, db *sql.DB) (RewriteAutoTuneActiveModel, bool, error) {
	if db == nil {
		return RewriteAutoTuneActiveModel{}, false, nil
	}

	var raw string
	err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?;", metaKeyRewriteAutoTuneActiveModel).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return RewriteAutoTuneActiveModel{}, false, nil
	}
	if err != nil {
		return RewriteAutoTuneActiveModel{}, false, err
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return RewriteAutoTuneActiveModel{}, false, nil
	}

	var model RewriteAutoTuneActiveModel
	if err := json.Unmarshal([]byte(raw), &model); err != nil {
		return RewriteAutoTuneActiveModel{}, false, err
	}
	return model, true, nil
}

func SetRewriteAutoTuneActiveModel(ctx context.Context, db *sql.DB, model RewriteAutoTuneActiveModel) error {
	if db == nil {
		return nil
	}
	if model.PromotedAt.IsZero() {
		model.PromotedAt = time.Now().UTC()
	} else {
		model.PromotedAt = model.PromotedAt.UTC()
	}

	payload, err := json.Marshal(model)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value;
`, metaKeyRewriteAutoTuneActiveModel, string(payload))
	return err
}
