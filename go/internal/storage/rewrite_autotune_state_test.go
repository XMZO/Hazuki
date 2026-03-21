package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestGetRewriteAutoTuneActiveModelMissing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hazuki.db")

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	model, ok, err := GetRewriteAutoTuneActiveModel(context.Background(), db)
	if err != nil {
		t.Fatalf("GetRewriteAutoTuneActiveModel: %v", err)
	}
	if ok {
		t.Fatalf("expected no model, got %+v", model)
	}
}

func TestRewriteAutoTuneActiveModelRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hazuki.db")

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	promotedAt := time.Date(2026, time.March, 21, 19, 8, 7, 0, time.FixedZone("UTC+8", 8*60*60))
	want := RewriteAutoTuneActiveModel{
		AdmissionIntercept: 0.241,
		AdmissionSlope:     -0.053,
		PromotedAt:         promotedAt,
	}

	if err := SetRewriteAutoTuneActiveModel(context.Background(), db, want); err != nil {
		t.Fatalf("SetRewriteAutoTuneActiveModel: %v", err)
	}

	got, ok, err := GetRewriteAutoTuneActiveModel(context.Background(), db)
	if err != nil {
		t.Fatalf("GetRewriteAutoTuneActiveModel: %v", err)
	}
	if !ok {
		t.Fatalf("expected persisted model")
	}
	if got.AdmissionIntercept != want.AdmissionIntercept {
		t.Fatalf("AdmissionIntercept = %.3f, want %.3f", got.AdmissionIntercept, want.AdmissionIntercept)
	}
	if got.AdmissionSlope != want.AdmissionSlope {
		t.Fatalf("AdmissionSlope = %.3f, want %.3f", got.AdmissionSlope, want.AdmissionSlope)
	}
	if !got.PromotedAt.Equal(promotedAt.UTC()) {
		t.Fatalf("PromotedAt = %s, want %s", got.PromotedAt.Format(time.RFC3339), promotedAt.UTC().Format(time.RFC3339))
	}
}
