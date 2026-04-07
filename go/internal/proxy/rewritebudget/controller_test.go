package rewritebudget

import (
	"runtime/debug"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want int64
		ok   bool
	}{
		{raw: "256MiB", want: 256 << 20, ok: true},
		{raw: "1.5GiB", want: int64(1.5 * float64(1<<30)), ok: true},
		{raw: "128mb", want: 128_000_000, ok: true},
		{raw: "4096", want: 4096, ok: true},
		{raw: "off", ok: false},
		{raw: "wat", ok: false},
	}

	for _, tt := range tests {
		got, ok := parseByteSize(tt.raw)
		if ok != tt.ok {
			t.Fatalf("parseByteSize(%q) ok = %v, want %v", tt.raw, ok, tt.ok)
		}
		if ok && got != tt.want {
			t.Fatalf("parseByteSize(%q) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

func TestDeriveAdaptiveGCPercentShrinksWithPressure(t *testing.T) {
	t.Parallel()

	baseGC := 80
	minGC := deriveAdaptiveMinGCPercent(baseGC)
	maxGC := deriveAdaptiveMaxGCPercent(baseGC)

	relaxed, _ := deriveAdaptiveGCPercent(0.10, baseGC, minGC, maxGC)
	balanced, _ := deriveAdaptiveGCPercent(0.50, baseGC, minGC, maxGC)
	emergency, _ := deriveAdaptiveGCPercent(0.95, baseGC, minGC, maxGC)

	if !(relaxed >= balanced && balanced >= emergency) {
		t.Fatalf("expected GC target to shrink with pressure, got relaxed=%d balanced=%d emergency=%d", relaxed, balanced, emergency)
	}
}

func TestPredictiveRewriteGuardBytesGrowsWithInflightWeight(t *testing.T) {
	t.Parallel()

	light := MemoryStatus{
		MemoryBudgetBytes:        256 << 20,
		EffectiveUsedBytes:       96 << 20,
		ActiveRewriteWeightBytes: 16 << 20,
		AdaptivePressureMilli:    150,
	}
	heavy := light
	heavy.ActiveRewriteWeightBytes = 64 << 20
	heavy.AdaptivePressureMilli = 700

	lightGuard := PredictiveRewriteGuardBytes(light, 2)
	heavyGuard := PredictiveRewriteGuardBytes(heavy, 2)
	if heavyGuard <= lightGuard {
		t.Fatalf("expected heavier in-flight load to increase guard bytes, got %d <= %d", heavyGuard, lightGuard)
	}
}

func TestRuntimePressureIndexTracksAdaptivePressure(t *testing.T) {
	t.Parallel()

	quiet := MemoryStatus{
		MemoryBudgetBytes:     256 << 20,
		EffectiveUsedBytes:    96 << 20,
		AdaptivePressureMilli: 100,
	}
	hot := quiet
	hot.EffectiveUsedBytes = 220 << 20
	hot.ActiveRewriteWeightBytes = 48 << 20
	hot.AdaptivePressureMilli = 900

	if RuntimePressureIndex(hot) <= RuntimePressureIndex(quiet) {
		t.Fatalf("expected hot memory status to have higher runtime pressure")
	}
}

func TestBufferedAdmissionLimitShrinksUnderPressure(t *testing.T) {
	t.Parallel()

	quiet := MemoryStatus{
		MemoryBudgetBytes:  256 << 20,
		EffectiveUsedBytes: 96 << 20,
	}
	hot := quiet
	hot.EffectiveUsedBytes = 220 << 20
	hot.ActiveRewriteWeightBytes = 48 << 20
	hot.AdaptivePressureMilli = 900

	quietLimit, quietEnabled := bufferedAdmissionLimitFromStatus(quiet)
	hotLimit, hotEnabled := bufferedAdmissionLimitFromStatus(hot)
	if !quietEnabled || !hotEnabled {
		t.Fatalf("expected buffered admission to be enabled when memory budget exists")
	}
	if hotLimit >= quietLimit {
		t.Fatalf("expected hot limit to shrink, got %d >= %d", hotLimit, quietLimit)
	}
}

func TestAcquireBufferedAdmissionFallsBackWhenPoolIsFull(t *testing.T) {
	bufferedAdmissionInUseBytes.Store(0)
	bufferedAdmissionFallbacks.Store(0)
	defer bufferedAdmissionInUseBytes.Store(0)
	defer bufferedAdmissionFallbacks.Store(0)

	prevPressure := adaptiveGCPressure.Load()
	adaptiveGCPressure.Store(0)
	defer adaptiveGCPressure.Store(prevPressure)

	prevMemoryLimit := debug.SetMemoryLimit(128 << 20)
	defer debug.SetMemoryLimit(prevMemoryLimit)

	release, ok := AcquireBufferedAdmission(16 << 20)
	if !ok || release == nil {
		t.Fatalf("expected first buffered admission to succeed")
	}
	defer release()

	if _, ok := AcquireBufferedAdmission(32 << 20); ok {
		t.Fatalf("expected second buffered admission to fall back under the pool limit")
	}
	if got := CurrentBufferedAdmissionFallbacks(); got < 1 {
		t.Fatalf("expected fallback counter to increase, got %d", got)
	}
}

func TestRuntimeTraceTunerLearnsPredictiveUtilization(t *testing.T) {
	t.Parallel()

	tuner := newRuntimeTraceTuner()
	prev := MemoryStatus{
		MemoryBudgetBytes:           100,
		EffectiveUsedBytes:          50,
		ActiveRewriteWeightBytes:    20,
		BufferedAdmissionInUseBytes: 8,
	}
	next := MemoryStatus{
		MemoryBudgetBytes:           100,
		EffectiveUsedBytes:          82,
		ActiveRewriteWeightBytes:    8,
		BufferedAdmissionInUseBytes: 2,
	}

	for i := 0; i < 24; i++ {
		tuner.Observe(prev, 0.35)
		tuner.Observe(next, 0.70)
	}

	snap := tuner.Snapshot()
	if snap.futureUtilSamples < 8 {
		t.Fatalf("expected future utilization model to learn, got %d samples", snap.futureUtilSamples)
	}

	predicted := clampFloat64(snap.futureUtilIntercept+snap.futureUtilSlope*baseProjectedUtilizationRatio(prev)+snap.futureUtilResidualP90, 0, 1.5)
	if predicted < 0.75 {
		t.Fatalf("expected learned predictive utilization >= 0.75, got %.3f", predicted)
	}
}

func TestRuntimeTraceTunerLearnsAdmissionShareReductionAfterHotOutcome(t *testing.T) {
	t.Parallel()

	tuner := newRuntimeTraceTuner()
	prev := MemoryStatus{
		MemoryBudgetBytes:           100,
		EffectiveUsedBytes:          58,
		BufferedAdmissionInUseBytes: 18,
		BufferedAdmissionLimitBytes: 24,
	}
	hot := MemoryStatus{
		MemoryBudgetBytes:           100,
		EffectiveUsedBytes:          94,
		BufferedAdmissionInUseBytes: 6,
		BufferedAdmissionLimitBytes: 16,
		CgroupMaxEvents:             1,
	}

	for i := 0; i < 24; i++ {
		tuner.Observe(prev, 0.85)
		tuner.Observe(hot, 0.95)
	}

	snap := tuner.Snapshot()
	if snap.admissionSamples < 8 {
		t.Fatalf("expected admission share model to learn, got %d samples", snap.admissionSamples)
	}

	share := clampFloat64(snap.admissionIntercept+snap.admissionSlope*0.85, 0.16, 0.38)
	if share >= 0.30 {
		t.Fatalf("expected learned admission share to tighten under hot outcome, got %.3f", share)
	}
}

func TestReportAdmissionAutoTunePromotesLiveModel(t *testing.T) {
	// NOT parallel: mutates package-level adaptiveTraceTunerState and adaptiveAutoTuneSnapshot.
	adaptiveTraceTunerState = newRuntimeTraceTuner()
	adaptiveAutoTuneSnapshot.Store(admissionAutoTuneState{
		Reason:                   "boot",
		ActiveAdmissionIntercept: defaultAdmissionIntercept,
		ActiveAdmissionSlope:     defaultAdmissionSlope,
	})

	ReportAdmissionAutoTune(AdmissionAutoTuneReport{
		Enabled:                  true,
		Interval:                 10 * time.Minute,
		MinTraceSamples:          180,
		TraceSamples:             240,
		ObservationSamples:       239,
		Recommended:              true,
		Reason:                   "promote",
		TrainImprovementPct:      8.4,
		ValidationImprovementPct: 5.2,
		CandidateIntercept:       0.24,
		CandidateSlope:           -0.04,
		Promote:                  true,
	})

	status := CurrentRuntimeModelStatus()
	if !status.AutoTuneEnabled {
		t.Fatalf("expected auto tune to be enabled")
	}
	if status.AutoTuneReason != "promote" {
		t.Fatalf("expected promote reason, got %q", status.AutoTuneReason)
	}
	if status.ActiveAdmissionIntercept != 0.24 || status.ActiveAdmissionSlope != -0.04 {
		t.Fatalf("expected promoted candidate to become active, got intercept=%.3f slope=%.3f", status.ActiveAdmissionIntercept, status.ActiveAdmissionSlope)
	}
	if status.AdmissionIntercept != 0.24 || status.AdmissionSlope != -0.04 {
		t.Fatalf("expected live tuner snapshot to reset to promoted candidate, got intercept=%.3f slope=%.3f", status.AdmissionIntercept, status.AdmissionSlope)
	}
	if status.AutoTuneLastPromotedAt.IsZero() {
		t.Fatalf("expected promoted timestamp to be recorded")
	}
}

func TestReportAdmissionAutoTuneConfigOnlyDoesNotMarkRun(t *testing.T) {
	// NOT parallel: mutates package-level adaptiveAutoTuneSnapshot.
	adaptiveAutoTuneSnapshot.Store(admissionAutoTuneState{
		Reason:                   "persisted",
		ActiveAdmissionIntercept: 0.22,
		ActiveAdmissionSlope:     -0.06,
	})

	ReportAdmissionAutoTune(AdmissionAutoTuneReport{
		Enabled:         true,
		Interval:        10 * time.Minute,
		MinTraceSamples: 180,
		Reason:          "persisted",
	})

	status := CurrentRuntimeModelStatus()
	if !status.AutoTuneEnabled {
		t.Fatalf("expected auto tune to stay enabled")
	}
	if status.AutoTuneReason != "persisted" {
		t.Fatalf("expected persisted reason, got %q", status.AutoTuneReason)
	}
	if !status.AutoTuneLastRunAt.IsZero() {
		t.Fatalf("expected config-only report to leave last run empty, got %s", status.AutoTuneLastRunAt.Format(time.RFC3339))
	}
	if status.ActiveAdmissionIntercept != 0.22 || status.ActiveAdmissionSlope != -0.06 {
		t.Fatalf("expected active model to stay unchanged, got intercept=%.3f slope=%.3f", status.ActiveAdmissionIntercept, status.ActiveAdmissionSlope)
	}
}
