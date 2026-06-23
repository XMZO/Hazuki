package rewritebudget

import (
	"runtime/debug"
	"testing"
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

func TestDeriveAdaptiveGCPercentHonorsBaseAndShrinks(t *testing.T) {
	t.Parallel()

	baseGC := 80
	minGC := deriveAdaptiveMinGCPercent(baseGC)
	maxGC := baseGC // the governor never raises GC above the configured GOGC

	// An idle process must honour the configured GOGC instead of being pushed
	// up to 100 (the bug this rewrite fixes).
	idle, _ := deriveAdaptiveGCPercent(0, baseGC, minGC, maxGC)
	if idle != baseGC {
		t.Fatalf("idle GC target should equal base GOGC %d, got %d", baseGC, idle)
	}

	relaxed, _ := deriveAdaptiveGCPercent(0.10, baseGC, minGC, maxGC)
	balanced, _ := deriveAdaptiveGCPercent(0.50, baseGC, minGC, maxGC)
	emergency, _ := deriveAdaptiveGCPercent(0.95, baseGC, minGC, maxGC)

	if relaxed > baseGC {
		t.Fatalf("GC target must never exceed base %d, got %d", baseGC, relaxed)
	}
	if !(relaxed >= balanced && balanced >= emergency) {
		t.Fatalf("expected GC target to shrink with pressure, got relaxed=%d balanced=%d emergency=%d", relaxed, balanced, emergency)
	}
	if emergency >= baseGC {
		t.Fatalf("expected emergency target below base, got %d", emergency)
	}
}

func TestNextAdmissionShareAIMD(t *testing.T) {
	t.Parallel()

	// Additive increase when calm (low utilisation, low forecast).
	calm := nextAdmissionShare(0.20, 0.40, 0.50, false, 0, 0, 0, 0)
	if calm <= 0.20 || calm > 0.20+admissionAdditiveStep+1e-9 {
		t.Fatalf("expected a single additive increase when calm, got %.4f", calm)
	}

	// Multiplicative decrease on OOM regardless of utilisation.
	if oom := nextAdmissionShare(0.30, 0.10, 0.10, true, 0, 0, 1, 0); oom >= 0.30 {
		t.Fatalf("expected multiplicative decrease on OOM, got %.4f", oom)
	}

	// Strong back-off at high utilisation.
	if hi := nextAdmissionShare(0.30, 0.93, 0.95, true, 0, 0, 0, 0); hi >= 0.30 {
		t.Fatalf("expected back-off at high utilisation, got %.4f", hi)
	}

	// Idle GC noise: cgroup high/max events with NO rewrite in flight must not
	// trigger back-off (the kernel raises these during ordinary GC reclaim).
	if idle := nextAdmissionShare(0.20, 0.40, 0.50, false, 5, 5, 0, 0); idle < 0.20 {
		t.Fatalf("idle GC events should not cause back-off, got %.4f", idle)
	}

	// Bounds are respected.
	if lo := nextAdmissionShare(admissionShareMin, 0.99, 0.99, true, 0, 0, 1, 0); lo < admissionShareMin {
		t.Fatalf("share fell below min: %.4f", lo)
	}
	if up := nextAdmissionShare(admissionShareMax, 0.40, 0.50, false, 0, 0, 0, 0); up > admissionShareMax {
		t.Fatalf("share exceeded max: %.4f", up)
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

func TestBufferedAdmissionLimitShrinksWithHeadroom(t *testing.T) {
	t.Parallel()

	quiet := MemoryStatus{
		MemoryBudgetBytes:  256 << 20,
		EffectiveUsedBytes: 96 << 20,
	}
	hot := quiet
	hot.EffectiveUsedBytes = 220 << 20
	hot.ActiveRewriteWeightBytes = 48 << 20

	quietLimit, quietEnabled := bufferedAdmissionLimitFromStatus(quiet)
	hotLimit, hotEnabled := bufferedAdmissionLimitFromStatus(hot)
	if !quietEnabled || !hotEnabled {
		t.Fatalf("expected buffered admission to be enabled when memory budget exists")
	}
	if hotLimit >= quietLimit {
		t.Fatalf("expected limit to shrink as headroom shrinks, got %d >= %d", hotLimit, quietLimit)
	}
}

func TestAcquireBufferedAdmissionFallsBackWhenPoolIsFull(t *testing.T) {
	bufferedAdmissionInUseBytes.Store(0)
	bufferedAdmissionFallbacks.Store(0)
	defer bufferedAdmissionInUseBytes.Store(0)
	defer bufferedAdmissionFallbacks.Store(0)

	prevShare := adaptiveAdmissionShareMilli.Load()
	adaptiveAdmissionShareMilli.Store(int64(admissionShareMax * 1000))
	defer adaptiveAdmissionShareMilli.Store(prevShare)

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
