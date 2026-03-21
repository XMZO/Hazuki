package app

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strings"
	"time"

	"hazuki-go/internal/proxy/rewritebudget"
	"hazuki-go/internal/storage"
	"hazuki-go/internal/tools/rewriteopt"
)

const (
	defaultRewriteAutoTuneInterval        = 10 * time.Minute
	defaultRewriteAutoTuneInitialDelay    = 2 * time.Minute
	defaultRewriteAutoTuneMinTraceSamples = 180
	defaultRewriteAutoTuneMaxTraceSamples = 256
	defaultRewriteAutoTuneIterations      = 24
	smallRewriteAutoTuneMemoryBudgetBytes = 768 << 20
	smallRewriteAutoTuneMaxTraceSamples   = 192
	smallRewriteAutoTuneIterations        = 16
	rewriteAutoTuneSkipPressureMilli      = 550
)

func restoreRewriteAutoTuneModel(ctx context.Context, db *sql.DB) {
	if db == nil || rewritebudget.HasAdmissionModelEnvOverride() {
		return
	}

	model, ok, err := storage.GetRewriteAutoTuneActiveModel(ctx, db)
	if err != nil {
		log.Printf("rewrite_autotune: restore persisted model failed: %v", err)
		return
	}
	if !ok {
		return
	}

	rewritebudget.RestoreAdmissionAutoTuneActiveModel(model.AdmissionIntercept, model.AdmissionSlope, model.PromotedAt)
	log.Printf("rewrite_autotune: restored active model intercept=%.4f slope=%.4f promoted_at=%s", model.AdmissionIntercept, model.AdmissionSlope, model.PromotedAt.UTC().Format(time.RFC3339))
}

func startRewriteAutoTune(ctx context.Context, db *sql.DB) {
	enabled := parseBoolDefault(os.Getenv("HAZUKI_REWRITE_AUTOTUNE"), true)
	interval := parseDurationDefault(os.Getenv("HAZUKI_REWRITE_AUTOTUNE_INTERVAL"), defaultRewriteAutoTuneInterval)
	minTraceSamples := parsePositiveInt(os.Getenv("HAZUKI_REWRITE_AUTOTUNE_MIN_TRACE_SAMPLES"), defaultRewriteAutoTuneMinTraceSamples)
	maxTraceSamples := parsePositiveInt(os.Getenv("HAZUKI_REWRITE_AUTOTUNE_MAX_TRACE_SAMPLES"), defaultRewriteAutoTuneMaxTraceSamples)
	iterations := parsePositiveInt(os.Getenv("HAZUKI_REWRITE_AUTOTUNE_ITERATIONS"), defaultRewriteAutoTuneIterations)
	if maxTraceSamples < minTraceSamples {
		maxTraceSamples = minTraceSamples
	}
	reason := "warming_up"
	if rewritebudget.CurrentRuntimeModelStatus().AutoTuneReason == "persisted" {
		reason = "persisted"
	}

	rewritebudget.ReportAdmissionAutoTune(rewritebudget.AdmissionAutoTuneReport{
		Enabled:         enabled,
		Interval:        interval,
		MinTraceSamples: minTraceSamples,
		Reason:          reason,
	})
	if !enabled {
		rewritebudget.ReportAdmissionAutoTune(rewritebudget.AdmissionAutoTuneReport{
			Enabled:         false,
			Interval:        interval,
			MinTraceSamples: minTraceSamples,
			Reason:          "disabled",
		})
		return
	}

	go runRewriteAutoTuneLoop(ctx, db, interval, minTraceSamples, maxTraceSamples, iterations)
}

func runRewriteAutoTuneLoop(ctx context.Context, db *sql.DB, interval time.Duration, minTraceSamples, maxTraceSamples, iterations int) {
	initialDelay := minDuration(defaultRewriteAutoTuneInitialDelay, interval)
	if initialDelay <= 0 {
		initialDelay = time.Minute
	}

	timer := time.NewTimer(initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runRewriteAutoTune(ctx, db, interval, minTraceSamples, maxTraceSamples, iterations)
			timer.Reset(interval)
		}
	}
}

func runRewriteAutoTune(ctx context.Context, db *sql.DB, interval time.Duration, minTraceSamples, maxTraceSamples, iterations int) {
	maxTraceSamples, iterations = runtimeRewriteAutoTuneBudget(maxTraceSamples, iterations)

	trace := rewritebudget.ExportRuntimeTrace()
	if maxTraceSamples > 0 && len(trace) > maxTraceSamples {
		trace = trace[len(trace)-maxTraceSamples:]
	}
	traceSamples := len(trace)
	observationSamples := 0
	if traceSamples > 0 {
		observationSamples = traceSamples - 1
	}

	if traceSamples < minTraceSamples {
		rewritebudget.ReportAdmissionAutoTune(rewritebudget.AdmissionAutoTuneReport{
			Enabled:            true,
			Interval:           interval,
			MinTraceSamples:    minTraceSamples,
			Run:                true,
			TraceSamples:       traceSamples,
			ObservationSamples: observationSamples,
			Reason:             "warming_up",
		})
		return
	}

	if reason, skip := shouldSkipRewriteAutoTune(); skip {
		rewritebudget.ReportAdmissionAutoTune(rewritebudget.AdmissionAutoTuneReport{
			Enabled:            true,
			Interval:           interval,
			MinTraceSamples:    minTraceSamples,
			Run:                true,
			TraceSamples:       traceSamples,
			ObservationSamples: observationSamples,
			Reason:             reason,
		})
		return
	}

	model := rewritebudget.CurrentRuntimeModelStatus()
	baseline := rewriteopt.DefaultCandidate()
	if model.ActiveAdmissionIntercept > 0 {
		baseline.AdmissionIntercept = model.ActiveAdmissionIntercept
		baseline.AdmissionSlope = model.ActiveAdmissionSlope
	}

	result := rewriteopt.Optimize(trace, baseline, iterations)
	report := rewritebudget.AdmissionAutoTuneReport{
		Enabled:                  true,
		Interval:                 interval,
		MinTraceSamples:          minTraceSamples,
		Run:                      true,
		TraceSamples:             traceSamples,
		ObservationSamples:       result.Observations,
		Recommended:              result.Gate.Recommended,
		Reason:                   result.Gate.Reason,
		TrainImprovementPct:      result.Gate.TrainImprovementPct,
		ValidationImprovementPct: result.Gate.ValidationImprovementPct,
		RiskIncreasePct:          result.Gate.RiskIncreasePct,
		CandidateIntercept:       result.Best.AdmissionIntercept,
		CandidateSlope:           result.Best.AdmissionSlope,
	}

	if result.Gate.Recommended {
		if hasMaterialCandidateDelta(baseline, result.Best) {
			if err := storage.SetRewriteAutoTuneActiveModel(ctx, db, storage.RewriteAutoTuneActiveModel{
				AdmissionIntercept: result.Best.AdmissionIntercept,
				AdmissionSlope:     result.Best.AdmissionSlope,
				PromotedAt:         time.Now().UTC(),
			}); err != nil {
				log.Printf("rewrite_autotune: persist promoted model failed: %v", err)
				report.Reason = "promote_memory_only"
			}
			report.Promote = true
			log.Printf("rewrite_autotune: promoted active model intercept=%.4f slope=%.4f train=%.2f%% validation=%.2f%%", result.Best.AdmissionIntercept, result.Best.AdmissionSlope, result.Gate.TrainImprovementPct, result.Gate.ValidationImprovementPct)
		} else {
			report.Recommended = false
			report.Reason = "no_material_change"
		}
	}

	rewritebudget.ReportAdmissionAutoTune(report)
}

func shouldSkipRewriteAutoTune() (string, bool) {
	status := rewritebudget.CurrentMemoryStatus()
	governor := rewritebudget.CurrentGovernorStatus()

	if governor.Mode == "guarded" || governor.Mode == "emergency" {
		return "busy", true
	}
	if governor.PressureMilli >= rewriteAutoTuneSkipPressureMilli {
		return "busy", true
	}
	if status.CgroupMaxEvents > 0 || status.CgroupOOMEvents > 0 || status.CgroupOOMKillEvents > 0 {
		return "busy", true
	}
	if rewritebudget.RuntimePressureIndex(status) >= 1.15 {
		return "busy", true
	}
	return "", false
}

func runtimeRewriteAutoTuneBudget(maxTraceSamples, iterations int) (int, int) {
	if maxTraceSamples <= 0 {
		maxTraceSamples = defaultRewriteAutoTuneMaxTraceSamples
	}
	if iterations <= 0 {
		iterations = defaultRewriteAutoTuneIterations
	}

	status := rewritebudget.CurrentMemoryStatus()
	if status.MemoryBudgetBytes > 0 && status.MemoryBudgetBytes <= smallRewriteAutoTuneMemoryBudgetBytes {
		if maxTraceSamples > smallRewriteAutoTuneMaxTraceSamples {
			maxTraceSamples = smallRewriteAutoTuneMaxTraceSamples
		}
		if iterations > smallRewriteAutoTuneIterations {
			iterations = smallRewriteAutoTuneIterations
		}
	}
	return maxTraceSamples, iterations
}

func hasMaterialCandidateDelta(current, next rewriteopt.Candidate) bool {
	return absFloat64(current.AdmissionIntercept-next.AdmissionIntercept) >= 0.004 ||
		absFloat64(current.AdmissionSlope-next.AdmissionSlope) >= 0.008
}

func parseBoolDefault(value string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(value))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	case "0", "false", "no", "off", "disable", "disabled":
		return false
	default:
		return fallback
	}
}

func parseDurationDefault(value string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 || a < b {
		return a
	}
	return b
}

func absFloat64(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
