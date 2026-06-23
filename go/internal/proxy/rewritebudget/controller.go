package rewritebudget

import (
	"context"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hazuki-go/internal/proxy/adaptmodel"
)

const (
	defaultAdaptiveGCPercent       = 100
	adaptiveGCTickInterval         = 2 * time.Second
	adaptiveGCFreeOSMemoryCooldown = 30 * time.Second
)

// AIMD admission control for buffered rewrite.
//
// The share of free headroom we are willing to hand to buffered (in-memory)
// rewrite is governed by an additive-increase / multiplicative-decrease loop,
// exactly like TCP congestion control:
//
//   - the objective is observable and external: avoid memory pressure
//     (high utilisation, cgroup high/max/oom events) — NOT "match our own
//     past usage", so there is no self-referential bootstrap;
//   - when things are calm we probe upward slowly (additive increase);
//   - when pressure appears we back off fast (multiplicative decrease).
//
// This replaces the previous RLS + Gaussian-process + offline-autotune stack,
// which optimised a target that was bootstrapped from the controller's own
// limited history and went idle whenever there was no rewrite traffic.
const (
	admissionShareMin     = 0.10
	admissionShareMax     = 0.34
	admissionShareInit    = 0.20
	admissionAdditiveStep = 0.01 // additive increase per calm tick
	admissionDecreaseHigh = 0.85 // mild back-off
	admissionDecreaseMax  = 0.70 // strong back-off
	admissionDecreaseOOM  = 0.50 // emergency back-off
)

const (
	governorModeOff = iota
	governorModeRelaxed
	governorModeBalanced
	governorModeGuarded
	governorModeEmergency
)

type GovernorStatus struct {
	Enabled             bool
	BaseGCPercent       int
	MinGCPercent        int
	MaxGCPercent        int
	CurrentGCPercent    int
	PressureMilli       int
	TraceSamples        int64
	PredictedUtilMilli  int
	AdmissionShareMilli int
	Mode                string
	LastAdjustedAt      time.Time
}

type RuntimeTraceRecord struct {
	TimeUnix                    int64  `json:"timeUnix"`
	Mode                        string `json:"mode"`
	CurrentGCPercent            int    `json:"currentGCPercent"`
	MemoryBudgetBytes           int64  `json:"memoryBudgetBytes"`
	GoMemoryUsedBytes           int64  `json:"goMemoryUsedBytes"`
	CgroupCurrentBytes          int64  `json:"cgroupCurrentBytes"`
	EffectiveUsedBytes          int64  `json:"effectiveUsedBytes"`
	ActiveRewrites              int64  `json:"activeRewrites"`
	ActiveRewriteWeightBytes    int64  `json:"activeRewriteWeightBytes"`
	BufferedAdmissionLimitBytes int64  `json:"bufferedAdmissionLimitBytes"`
	BufferedAdmissionInUseBytes int64  `json:"bufferedAdmissionInUseBytes"`
	BufferedAdmissionFallbacks  int64  `json:"bufferedAdmissionFallbacks"`
	AdaptivePressureMilli       int    `json:"adaptivePressureMilli"`
	PredictedUtilMilli          int    `json:"predictedUtilMilli"`
	LearnedAdmissionShareMilli  int    `json:"learnedAdmissionShareMilli"`
	CgroupHighEvents            int64  `json:"cgroupHighEvents"`
	CgroupMaxEvents             int64  `json:"cgroupMaxEvents"`
	CgroupOOMEvents             int64  `json:"cgroupOOMEvents"`
	CgroupOOMKillEvents         int64  `json:"cgroupOOMKillEvents"`
}

type adaptiveGCState struct {
	pressure          float64
	currentGCPercent  int
	lastHighEvents    int64
	lastMaxEvents     int64
	lastOOMEvents     int64
	lastOOMKillEvents int64
	lastFreeOSMemory  time.Time

	admissionShare   float64
	prevProjected    float64
	hasPrevProjected bool
}

type runtimeTraceBuffer struct {
	mu      sync.Mutex
	records []RuntimeTraceRecord
	next    int
	full    bool
}

var (
	runtimeEnvApplyOnce         sync.Once
	adaptiveGCStarted           atomic.Bool
	adaptiveGCEnabled           atomic.Bool
	adaptiveGCBase              atomic.Int64
	adaptiveGCMin               atomic.Int64
	adaptiveGCMax               atomic.Int64
	adaptiveGCCurrent           atomic.Int64
	adaptiveGCPressure          atomic.Int64
	adaptiveGCMode              atomic.Int64
	adaptiveGCLastSet           atomic.Int64
	adaptiveTraceSamples        atomic.Int64
	adaptivePredictedUtilMilli  atomic.Int64
	adaptiveAdmissionShareMilli atomic.Int64
	adaptiveFutureResidualMilli atomic.Int64
	adaptiveTraceBuffer         = newRuntimeTraceBuffer(2048)

	// futureUtilResidualP90 estimates the P90 of the observed forecast error
	// (actual utilisation minus projected utilisation) and is used purely as a
	// safety margin on the predictive utilisation. It is written only by the
	// single governor goroutine; readers go through adaptiveFutureResidualMilli.
	futureUtilResidualP90 = adaptmodel.NewP2Quantile(0.90)
)

func newRuntimeTraceBuffer(size int) *runtimeTraceBuffer {
	if size <= 0 {
		size = 256
	}
	return &runtimeTraceBuffer{records: make([]RuntimeTraceRecord, size)}
}

func (b *runtimeTraceBuffer) Append(record RuntimeTraceRecord) {
	if b == nil || len(b.records) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records[b.next] = record
	b.next++
	if b.next >= len(b.records) {
		b.next = 0
		b.full = true
	}
}

func (b *runtimeTraceBuffer) Snapshot() []RuntimeTraceRecord {
	if b == nil || len(b.records) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.full {
		out := make([]RuntimeTraceRecord, b.next)
		copy(out, b.records[:b.next])
		return out
	}

	out := make([]RuntimeTraceRecord, len(b.records))
	n := copy(out, b.records[b.next:])
	copy(out[n:], b.records[:b.next])
	return out
}

func ExportRuntimeTrace() []RuntimeTraceRecord {
	return adaptiveTraceBuffer.Snapshot()
}

func CurrentGovernorStatus() GovernorStatus {
	lastAdjustedAt := time.Time{}
	if unix := adaptiveGCLastSet.Load(); unix > 0 {
		lastAdjustedAt = time.Unix(unix, 0).UTC()
	}
	return GovernorStatus{
		Enabled:             adaptiveGCEnabled.Load(),
		BaseGCPercent:       clampPositiveInt64ToInt(adaptiveGCBase.Load()),
		MinGCPercent:        clampPositiveInt64ToInt(adaptiveGCMin.Load()),
		MaxGCPercent:        clampPositiveInt64ToInt(adaptiveGCMax.Load()),
		CurrentGCPercent:    clampPositiveInt64ToInt(adaptiveGCCurrent.Load()),
		PressureMilli:       clampNonNegativeInt64ToInt(adaptiveGCPressure.Load()),
		TraceSamples:        maxInt64Value(adaptiveTraceSamples.Load(), 0),
		PredictedUtilMilli:  clampNonNegativeInt64ToInt(adaptivePredictedUtilMilli.Load()),
		AdmissionShareMilli: clampNonNegativeInt64ToInt(adaptiveAdmissionShareMilli.Load()),
		Mode:                governorModeString(adaptiveGCMode.Load()),
		LastAdjustedAt:      lastAdjustedAt,
	}
}

func currentAdaptivePressureMilli() int {
	return clampNonNegativeInt64ToInt(adaptiveGCPressure.Load())
}

func RuntimePressureIndex(status MemoryStatus) float64 {
	pressure := 0.0
	utilization := currentUtilizationRatio(status)
	predictiveUtilization := predictiveUtilizationRatio(status)
	if status.MemoryBudgetBytes > 0 && status.EffectiveUsedBytes > 0 {
		if utilization > 0.70 {
			pressure += clampFloat64((utilization-0.70)/0.30, 0, 2.0)
		}
		if predictiveUtilization > 0.78 {
			pressure += clampFloat64((predictiveUtilization-0.78)/0.22, 0, 0.8)
		}
		if status.ActiveRewriteWeightBytes > 0 {
			weightRatio := float64(status.ActiveRewriteWeightBytes) / float64(status.MemoryBudgetBytes)
			if weightRatio > 0.10 {
				pressure += clampFloat64((weightRatio-0.10)/0.30, 0, 0.6)
			}
		}
	}
	// Use log1p to attenuate cumulative-since-startup event counters: the
	// first few events have significant impact, but hundreds of events
	// (common in long-running memory-limited containers) plateau at the cap
	// instead of saturating immediately with a linear scale.
	if status.CgroupHighEvents > 0 {
		pressure += clampFloat64(0.10*math.Log1p(float64(status.CgroupHighEvents)), 0, 0.25)
	}
	if status.CgroupMaxEvents > 0 {
		pressure += clampFloat64(0.12*math.Log1p(float64(status.CgroupMaxEvents)), 0, 0.35)
	}
	if status.CgroupOOMEvents > 0 || status.CgroupOOMKillEvents > 0 {
		pressure += 0.6
	}
	if status.AdaptivePressureMilli > 0 {
		pressure = math.Max(pressure, clampFloat64(float64(status.AdaptivePressureMilli)/1000*2.4, 0, 2.4))
	}
	return clampFloat64(pressure, 0, 3)
}

func PredictiveRewriteGuardBytes(status MemoryStatus, activeRewrites int64) int64 {
	if status.ActiveRewriteWeightBytes <= 0 || activeRewrites <= 0 {
		return 0
	}
	avgWeight := status.ActiveRewriteWeightBytes / activeRewrites
	if avgWeight <= 0 {
		return 0
	}
	factor := 0.30 + 0.25*RuntimePressureIndex(status)
	return clampFloat64ToInt64(float64(avgWeight) * clampFloat64(factor, 0.30, 0.95))
}

func AcquireBufferedAdmission(weightBytes int64) (func(), bool) {
	if weightBytes <= 0 {
		return func() {}, true
	}

	status := CurrentMemoryStatus()
	limitBytes, enabled := bufferedAdmissionLimitFromStatus(status)
	if !enabled {
		return func() {}, true
	}
	if weightBytes > limitBytes {
		bufferedAdmissionFallbacks.Add(1)
		return nil, false
	}

	for {
		current := CurrentBufferedAdmissionInUseBytes()
		if current+weightBytes > limitBytes {
			bufferedAdmissionFallbacks.Add(1)
			return nil, false
		}
		if bufferedAdmissionInUseBytes.CompareAndSwap(current, current+weightBytes) {
			return func() {
				bufferedAdmissionInUseBytes.Add(-weightBytes)
			}, true
		}
	}
}

// currentAdmissionShare returns the AIMD-controlled share of headroom that may
// be handed to buffered rewrite.
func currentAdmissionShare() float64 {
	v := adaptiveAdmissionShareMilli.Load()
	if v <= 0 {
		return admissionShareInit
	}
	return clampFloat64(float64(v)/1000, admissionShareMin, admissionShareMax)
}

func bufferedAdmissionLimitFromStatus(status MemoryStatus) (int64, bool) {
	if status.MemoryBudgetBytes <= 0 || status.EffectiveUsedBytes < 0 || status.MemoryBudgetBytes <= status.EffectiveUsedBytes {
		return 0, false
	}

	headroomBytes := status.MemoryBudgetBytes - status.EffectiveUsedBytes
	if headroomBytes <= 0 {
		return 0, true
	}

	share := currentAdmissionShare()
	limitBytes := clampFloat64ToInt64(float64(headroomBytes) * clampFloat64(share, admissionShareMin, admissionShareMax))
	hardCapBytes := clampInt64(status.MemoryBudgetBytes/5, 8<<20, 64<<20)
	if limitBytes > hardCapBytes {
		limitBytes = hardCapBytes
	}
	if status.CgroupMaxEvents > 0 || status.CgroupOOMEvents > 0 || status.CgroupOOMKillEvents > 0 {
		limitBytes = clampFloat64ToInt64(float64(limitBytes) * 0.75)
	}
	if limitBytes < 2<<20 {
		limitBytes = 2 << 20
	}
	return limitBytes, true
}

func ApplyRuntimeEnv() {
	runtimeEnvApplyOnce.Do(func() {
		if limit, ok := runtimeMemoryLimitFromEnv(); ok {
			debug.SetMemoryLimit(limit)
		}
		if gc, ok, disabled := runtimeGCPercentFromEnv(); disabled {
			adaptiveGCEnabled.Store(false)
			adaptiveGCMode.Store(governorModeOff)
		} else if ok {
			debug.SetGCPercent(gc)
		}
	})
}

func StartAdaptiveGCController(ctx context.Context) {
	ApplyRuntimeEnv()
	if !adaptiveGCStarted.CompareAndSwap(false, true) {
		return
	}

	baseGC, enabled := detectedAdaptiveBaseGCPercent()
	if baseGC <= 0 {
		baseGC = defaultAdaptiveGCPercent
	}
	minGC := deriveAdaptiveMinGCPercent(baseGC)
	// The base GC percent (the user's configured GOGC) is the relaxed ceiling.
	// Under pressure we only ever tighten toward minGC; we never raise GC above
	// the configured value, so an idle process honours GOGC instead of being
	// silently pushed up to 100 (the previous behaviour, which defeated a
	// low GOGC chosen to save memory).
	maxGC := baseGC

	adaptiveGCBase.Store(int64(baseGC))
	adaptiveGCMin.Store(int64(minGC))
	adaptiveGCMax.Store(int64(maxGC))
	adaptiveGCCurrent.Store(int64(baseGC))
	adaptiveGCLastSet.Store(time.Now().Unix())
	adaptiveAdmissionShareMilli.Store(int64(math.Round(admissionShareInit * 1000)))

	if !enabled {
		adaptiveGCEnabled.Store(false)
		adaptiveGCMode.Store(governorModeOff)
		return
	}

	adaptiveGCEnabled.Store(true)
	adaptiveGCMode.Store(governorModeBalanced)
	debug.SetGCPercent(baseGC)

	go runAdaptiveGCController(ctx, adaptiveGCState{currentGCPercent: baseGC, admissionShare: admissionShareInit})
}

func runAdaptiveGCController(ctx context.Context, state adaptiveGCState) {
	ticker := time.NewTicker(adaptiveGCTickInterval)
	defer ticker.Stop()

	applyAdaptiveGCStatus(state.currentGCPercent, 0, governorModeBalanced)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			memory := CurrentMemoryStatus()

			// Per-tick deltas of the cumulative cgroup event counters.
			highDelta := positiveDelta(memory.CgroupHighEvents, state.lastHighEvents)
			maxDelta := positiveDelta(memory.CgroupMaxEvents, state.lastMaxEvents)
			oomDelta := positiveDelta(memory.CgroupOOMEvents, state.lastOOMEvents)
			oomKillDelta := positiveDelta(memory.CgroupOOMKillEvents, state.lastOOMKillEvents)
			state.lastHighEvents = memory.CgroupHighEvents
			state.lastMaxEvents = memory.CgroupMaxEvents
			state.lastOOMEvents = memory.CgroupOOMEvents
			state.lastOOMKillEvents = memory.CgroupOOMKillEvents

			// Update the predictive-utilisation safety margin (P90 of the
			// forecast residual). This observes a real quantity (actual vs
			// projected utilisation), so it does not bootstrap from policy.
			util := currentUtilizationRatio(memory)
			base := baseProjectedUtilizationRatio(memory)
			if state.hasPrevProjected {
				futureUtilResidualP90.Observe(maxFloat64(util-state.prevProjected, 0))
			}
			state.prevProjected = base
			state.hasPrevProjected = true
			residual := futureUtilResidualP90.Estimate(0)
			adaptiveFutureResidualMilli.Store(int64(clampInt(int(math.Round(residual*1000)), 0, 1500)))
			predictedUtil := clampFloat64(maxFloat64(base+residual, util), util, 1.5)

			// Pressure index (smoothed) drives the GC governor.
			pressure := sampleAdaptiveMemoryPressure(memory, highDelta, maxDelta, oomDelta, oomKillDelta)
			state.pressure = smoothAdaptivePressure(state.pressure, pressure)

			// AIMD admission control.
			hasInUse := memory.BufferedAdmissionInUseBytes > 0
			state.admissionShare = nextAdmissionShare(state.admissionShare, util, predictedUtil, hasInUse, highDelta, maxDelta, oomDelta, oomKillDelta)
			applyAdaptiveTraceStatus(predictedUtil, state.admissionShare, futureUtilResidualP90.Count())

			targetGC, mode := deriveAdaptiveGCPercent(state.pressure,
				clampPositiveInt64ToInt(adaptiveGCBase.Load()),
				clampPositiveInt64ToInt(adaptiveGCMin.Load()),
				clampPositiveInt64ToInt(adaptiveGCMax.Load()))
			if targetGC <= 0 {
				targetGC = clampPositiveInt64ToInt(adaptiveGCBase.Load())
			}
			if targetGC <= 0 {
				targetGC = defaultAdaptiveGCPercent
			}

			if targetGC != state.currentGCPercent {
				debug.SetGCPercent(targetGC)
				state.currentGCPercent = targetGC
			}
			if shouldForceFreeOSMemory(memory, state, mode) {
				debug.FreeOSMemory()
				state.lastFreeOSMemory = time.Now()
			}
			pressureMilli := clampInt(int(math.Round(state.pressure*1000)), 0, 1000)
			applyAdaptiveGCStatus(targetGC, pressureMilli, mode)
			appendRuntimeTrace(memory, targetGC, mode, pressureMilli, predictedUtil, state.admissionShare)
		}
	}
}

// nextAdmissionshare applies one AIMD step. The decrease signals are objective
// memory-pressure indicators; max/high cgroup events are only trusted when
// rewrite is actually holding memory, because the kernel routinely raises those
// during ordinary GC reclaim on a tight memory limit (they would otherwise
// cause spurious back-off on a completely idle process).
func nextAdmissionShare(share, util, predictedUtil float64, hasInUse bool, highDelta, maxDelta, oomDelta, oomKillDelta int64) float64 {
	if share <= 0 {
		share = admissionShareInit
	}
	switch {
	case oomDelta > 0 || oomKillDelta > 0:
		share *= admissionDecreaseOOM
	case util >= 0.92 || (hasInUse && maxDelta > 0):
		share *= admissionDecreaseMax
	case util >= 0.85 || predictedUtil >= 0.90 || (hasInUse && highDelta > 0):
		share *= admissionDecreaseHigh
	case util <= 0.70 && predictedUtil <= 0.78:
		share += admissionAdditiveStep
	}
	return clampFloat64(share, admissionShareMin, admissionShareMax)
}

func sampleAdaptiveMemoryPressure(memory MemoryStatus, highDelta, maxDelta, oomDelta, oomKillDelta int64) float64 {
	if memory.MemoryBudgetBytes <= 0 {
		if oomDelta > 0 || oomKillDelta > 0 {
			return 1
		}
		if maxDelta > 0 {
			return 0.8
		}
		if highDelta > 0 {
			return 0.55
		}
		return 0
	}

	utilization := currentUtilizationRatio(memory)
	predictiveUtilization := predictiveUtilizationRatio(memory)
	weightRatio := 0.0
	if memory.ActiveRewriteWeightBytes > 0 {
		weightRatio = float64(memory.ActiveRewriteWeightBytes) / float64(memory.MemoryBudgetBytes)
	}

	pressure := 0.0
	if utilization > 0.60 {
		pressure += clampFloat64((utilization-0.60)/0.28, 0, 0.55)
	}
	if utilization > 0.82 {
		pressure += clampFloat64((utilization-0.82)/0.12, 0, 0.25)
	}
	if predictiveUtilization > 0.72 {
		pressure += clampFloat64((predictiveUtilization-0.72)/0.20, 0, 0.25)
	}
	if weightRatio > 0.08 {
		pressure += clampFloat64((weightRatio-0.08)/0.24, 0, 0.18)
	}
	if highDelta > 0 {
		pressure += clampFloat64(0.10*float64(highDelta), 0, 0.20)
	}
	if maxDelta > 0 {
		pressure += clampFloat64(0.18*float64(maxDelta), 0, 0.36)
	}
	if oomDelta > 0 || oomKillDelta > 0 {
		pressure += 0.35
	}

	return clampFloat64(pressure, 0, 1)
}

func smoothAdaptivePressure(prev, next float64) float64 {
	next = clampFloat64(next, 0, 1)
	prev = clampFloat64(prev, 0, 1)
	alpha := 0.12
	if next > prev {
		alpha = 0.28
	}
	smoothed := prev + alpha*(next-prev)
	if next >= 0.90 {
		smoothed = math.Max(smoothed, next)
	}
	return clampFloat64(smoothed, 0, 1)
}

// deriveAdaptiveGCPercent maps pressure to a GC percent in [minGC, baseGC].
// At zero pressure the target is baseGC (the user's GOGC); rising pressure
// tightens monotonically toward minGC. It never exceeds baseGC.
func deriveAdaptiveGCPercent(pressure float64, baseGC, minGC, maxGC int) (int, int64) {
	if baseGC <= 0 {
		baseGC = defaultAdaptiveGCPercent
	}
	if minGC <= 0 {
		minGC = deriveAdaptiveMinGCPercent(baseGC)
	}
	if maxGC < baseGC {
		maxGC = baseGC
	}

	p := clampFloat64(pressure, 0, 1)
	mode := governorModeRelaxed
	switch {
	case p >= 0.82:
		mode = governorModeEmergency
	case p >= 0.55:
		mode = governorModeGuarded
	case p >= 0.30:
		mode = governorModeBalanced
	}

	target := baseGC - int(math.Round(float64(baseGC-minGC)*math.Pow(p, 1.3)))
	return alignGCPercent(clampInt(target, minGC, baseGC)), int64(mode)
}

func shouldForceFreeOSMemory(memory MemoryStatus, state adaptiveGCState, mode int64) bool {
	if mode != governorModeEmergency {
		return false
	}
	now := time.Now()
	if !state.lastFreeOSMemory.IsZero() && now.Sub(state.lastFreeOSMemory) < adaptiveGCFreeOSMemoryCooldown {
		return false
	}
	if memory.CgroupOOMEvents > 0 || memory.CgroupOOMKillEvents > 0 {
		return true
	}
	if memory.CgroupMaxEvents > 0 && memory.MemoryBudgetBytes > 0 {
		return predictiveUtilizationRatio(memory) >= 0.92
	}
	if memory.MemoryBudgetBytes <= 0 {
		if memory.CgroupMaxEvents > 0 {
			return true
		}
		return false
	}
	if predictiveUtilizationRatio(memory) >= 0.96 {
		return true
	}
	return false
}

func applyAdaptiveGCStatus(currentGC, pressureMilli int, mode int64) {
	adaptiveGCCurrent.Store(int64(currentGC))
	adaptiveGCPressure.Store(int64(clampInt(pressureMilli, 0, 1000)))
	adaptiveGCMode.Store(mode)
	adaptiveGCLastSet.Store(time.Now().Unix())
}

func applyAdaptiveTraceStatus(predictedUtilization, admissionShare float64, samples int64) {
	adaptiveTraceSamples.Store(maxInt64Value(samples, 0))
	adaptivePredictedUtilMilli.Store(int64(clampInt(int(math.Round(predictedUtilization*1000)), 0, 1500)))
	adaptiveAdmissionShareMilli.Store(int64(clampInt(int(math.Round(admissionShare*1000)), 0, 1000)))
}

func appendRuntimeTrace(status MemoryStatus, currentGC int, mode int64, pressureMilli int, predictedUtilization, admissionShare float64) {
	status.AdaptivePressureMilli = clampInt(pressureMilli, 0, 1000)
	if limitBytes, enabled := bufferedAdmissionLimitFromStatus(status); enabled {
		status.BufferedAdmissionLimitBytes = limitBytes
	}
	adaptiveTraceBuffer.Append(RuntimeTraceRecord{
		TimeUnix:                    time.Now().UTC().Unix(),
		Mode:                        governorModeString(mode),
		CurrentGCPercent:            currentGC,
		MemoryBudgetBytes:           status.MemoryBudgetBytes,
		GoMemoryUsedBytes:           status.GoMemoryUsedBytes,
		CgroupCurrentBytes:          status.CgroupCurrentBytes,
		EffectiveUsedBytes:          status.EffectiveUsedBytes,
		ActiveRewrites:              CurrentActiveCount(),
		ActiveRewriteWeightBytes:    status.ActiveRewriteWeightBytes,
		BufferedAdmissionLimitBytes: status.BufferedAdmissionLimitBytes,
		BufferedAdmissionInUseBytes: status.BufferedAdmissionInUseBytes,
		BufferedAdmissionFallbacks:  status.BufferedAdmissionFallbacks,
		AdaptivePressureMilli:       status.AdaptivePressureMilli,
		PredictedUtilMilli:          clampInt(int(math.Round(predictedUtilization*1000)), 0, 1500),
		LearnedAdmissionShareMilli:  clampInt(int(math.Round(admissionShare*1000)), 0, 1000),
		CgroupHighEvents:            status.CgroupHighEvents,
		CgroupMaxEvents:             status.CgroupMaxEvents,
		CgroupOOMEvents:             status.CgroupOOMEvents,
		CgroupOOMKillEvents:         status.CgroupOOMKillEvents,
	})
}

func currentUtilizationRatio(status MemoryStatus) float64 {
	if status.MemoryBudgetBytes <= 0 || status.EffectiveUsedBytes <= 0 {
		return 0
	}
	return clampFloat64(float64(status.EffectiveUsedBytes)/float64(status.MemoryBudgetBytes), 0, 1.5)
}

func baseProjectedUtilizationRatio(status MemoryStatus) float64 {
	utilization := currentUtilizationRatio(status)
	if status.MemoryBudgetBytes <= 0 {
		return utilization
	}
	projected := utilization
	if status.ActiveRewriteWeightBytes > 0 {
		projected += (float64(status.ActiveRewriteWeightBytes) / float64(status.MemoryBudgetBytes)) * 0.60
	}
	if status.BufferedAdmissionInUseBytes > 0 {
		projected += (float64(status.BufferedAdmissionInUseBytes) / float64(status.MemoryBudgetBytes)) * 0.25
	}
	return clampFloat64(projected, utilization, 1.5)
}

// predictiveUtilizationRatio is the projected utilisation plus the observed P90
// forecast residual (published as an atomic by the governor goroutine).
func predictiveUtilizationRatio(status MemoryStatus) float64 {
	utilization := currentUtilizationRatio(status)
	base := baseProjectedUtilizationRatio(status)
	residual := float64(adaptiveFutureResidualMilli.Load()) / 1000
	return clampFloat64(maxFloat64(base+residual, utilization), utilization, 1.5)
}

func detectedAdaptiveBaseGCPercent() (int, bool) {
	if value, ok, disabled := runtimeGCPercentFromEnv(); disabled {
		return defaultAdaptiveGCPercent, false
	} else if ok {
		return value, true
	}
	return defaultAdaptiveGCPercent, true
}

func deriveAdaptiveMinGCPercent(baseGC int) int {
	return clampInt(int(math.Round(float64(baseGC)*0.65)), 50, maxInt(baseGC, 80))
}

func alignGCPercent(value int) int {
	if value <= 0 {
		return defaultAdaptiveGCPercent
	}
	const step = 5
	return maxInt(step, ((value+(step/2))/step)*step)
}

func runtimeMemoryLimitFromEnv() (int64, bool) {
	for _, key := range []string{"GOMEMLIMIT", "HAZUKI_GOMEMLIMIT"} {
		raw, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		value := strings.TrimSpace(raw)
		if value == "" || strings.EqualFold(value, "off") {
			return 0, false
		}
		n, ok := parseByteSize(value)
		if !ok {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func runtimeGCPercentFromEnv() (int, bool, bool) {
	for _, key := range []string{"GOGC", "HAZUKI_GOGC"} {
		raw, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		value := strings.TrimSpace(raw)
		if value == "" {
			return 0, false, false
		}
		if strings.EqualFold(value, "off") {
			return 0, false, true
		}
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return 0, false, false
		}
		return n, true, false
	}
	return 0, false, false
}

func parseByteSize(raw string) (int64, bool) {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" {
		return 0, false
	}

	splitAt := 0
	for splitAt < len(value) {
		ch := value[splitAt]
		if (ch >= '0' && ch <= '9') || ch == '.' {
			splitAt++
			continue
		}
		break
	}
	if splitAt == 0 {
		return 0, false
	}

	numberPart := strings.TrimSpace(value[:splitAt])
	suffixPart := strings.TrimSpace(value[splitAt:])
	number, err := strconv.ParseFloat(numberPart, 64)
	if err != nil || number <= 0 {
		return 0, false
	}

	multiplier := float64(1)
	switch suffixPart {
	case "", "b":
		multiplier = 1
	case "k", "kb":
		multiplier = 1e3
	case "m", "mb":
		multiplier = 1e6
	case "g", "gb":
		multiplier = 1e9
	case "t", "tb":
		multiplier = 1e12
	case "ki", "kib":
		multiplier = 1 << 10
	case "mi", "mib":
		multiplier = 1 << 20
	case "gi", "gib":
		multiplier = 1 << 30
	case "ti", "tib":
		multiplier = 1 << 40
	default:
		return 0, false
	}

	result := number * multiplier
	if result <= 0 || result >= float64(maxInt64) {
		return 0, false
	}
	return int64(result), true
}

func governorModeString(code int64) string {
	switch code {
	case governorModeRelaxed:
		return "relaxed"
	case governorModeBalanced:
		return "balanced"
	case governorModeGuarded:
		return "guarded"
	case governorModeEmergency:
		return "emergency"
	default:
		return "off"
	}
}

func clampFloat64(value, minValue, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func clampFloat64ToInt64(value float64) int64 {
	if value <= 0 {
		return 0
	}
	if value >= float64(maxInt64) {
		return maxInt64
	}
	return int64(value)
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func clampInt64(value, minValue, maxValue int64) int64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func clampPositiveInt64ToInt(value int64) int {
	if value <= 0 {
		return 0
	}
	if value >= int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(value)
}

func clampNonNegativeInt64ToInt(value int64) int {
	if value <= 0 {
		return 0
	}
	return clampPositiveInt64ToInt(value)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
