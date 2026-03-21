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
	defaultAdmissionIntercept      = 0.34
	defaultAdmissionSlope          = -0.14
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

type RuntimeModelStatus struct {
	FutureUtilIntercept              float64   `json:"futureUtilIntercept"`
	FutureUtilSlope                  float64   `json:"futureUtilSlope"`
	FutureUtilResidualP90            float64   `json:"futureUtilResidualP90"`
	FutureUtilSamples                int64     `json:"futureUtilSamples"`
	AdmissionIntercept               float64   `json:"admissionIntercept"`
	AdmissionSlope                   float64   `json:"admissionSlope"`
	AdmissionSamples                 int64     `json:"admissionSamples"`
	PredictedUtilMilli               int       `json:"predictedUtilMilli"`
	LearnedAdmissionShareMilli       int       `json:"learnedAdmissionShareMilli"`
	AutoTuneEnabled                  bool      `json:"autoTuneEnabled"`
	AutoTuneIntervalSeconds          int64     `json:"autoTuneIntervalSeconds"`
	AutoTuneMinTraceSamples          int       `json:"autoTuneMinTraceSamples"`
	AutoTuneTraceSamples             int       `json:"autoTuneTraceSamples"`
	AutoTuneObservationSamples       int       `json:"autoTuneObservationSamples"`
	AutoTuneTrainImprovementPct      float64   `json:"autoTuneTrainImprovementPct"`
	AutoTuneValidationImprovementPct float64   `json:"autoTuneValidationImprovementPct"`
	AutoTuneRiskIncreasePct          float64   `json:"autoTuneRiskIncreasePct"`
	AutoTuneRecommended              bool      `json:"autoTuneRecommended"`
	AutoTuneReason                   string    `json:"autoTuneReason"`
	AutoTuneLastRunAt                time.Time `json:"autoTuneLastRunAt"`
	AutoTuneLastPromotedAt           time.Time `json:"autoTuneLastPromotedAt"`
	ActiveAdmissionIntercept         float64   `json:"activeAdmissionIntercept"`
	ActiveAdmissionSlope             float64   `json:"activeAdmissionSlope"`
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
}

type runtimeTraceSample struct {
	utilization         float64
	baseProjectedUtil   float64
	pressureNorm        float64
	headroomBytes       int64
	bufferedInUseBytes  int64
	bufferedLimitBytes  int64
	cgroupHighEvents    int64
	cgroupMaxEvents     int64
	cgroupOOMEvents     int64
	cgroupOOMKillEvents int64
}

type runtimeTraceSnapshot struct {
	futureUtilIntercept   float64
	futureUtilSlope       float64
	futureUtilResidualP90 float64
	futureUtilSamples     int64
	admissionIntercept    float64
	admissionSlope        float64
	admissionSamples      int64
}

type runtimeTraceTuner struct {
	mu                    sync.Mutex
	prev                  runtimeTraceSample
	hasPrev               bool
	futureUtilModel       adaptmodel.LinearRLS
	futureUtilResidualP90 adaptmodel.P2Quantile
	admissionShareModel   adaptmodel.LinearRLS
	snapshot              atomic.Value
}

type runtimeTraceBuffer struct {
	mu      sync.Mutex
	records []RuntimeTraceRecord
	next    int
	full    bool
}

type AdmissionAutoTuneReport struct {
	Enabled                  bool
	Interval                 time.Duration
	MinTraceSamples          int
	TraceSamples             int
	ObservationSamples       int
	Recommended              bool
	Reason                   string
	TrainImprovementPct      float64
	ValidationImprovementPct float64
	RiskIncreasePct          float64
	CandidateIntercept       float64
	CandidateSlope           float64
	Promote                  bool
}

type admissionAutoTuneState struct {
	Enabled                  bool
	IntervalSeconds          int64
	MinTraceSamples          int
	TraceSamples             int
	ObservationSamples       int
	TrainImprovementPct      float64
	ValidationImprovementPct float64
	RiskIncreasePct          float64
	Recommended              bool
	Reason                   string
	LastRunAt                time.Time
	LastPromotedAt           time.Time
	ActiveAdmissionIntercept float64
	ActiveAdmissionSlope     float64
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
	adaptiveTraceTunerState     = newRuntimeTraceTuner()
	adaptiveTraceBuffer         = newRuntimeTraceBuffer(2048)
	adaptiveAutoTuneMu          sync.Mutex
	adaptiveAutoTuneSnapshot    atomic.Value
)

func init() {
	intercept, slope := runtimeAdmissionModelFromEnv()
	adaptiveAutoTuneSnapshot.Store(admissionAutoTuneState{
		Reason:                   "boot",
		ActiveAdmissionIntercept: intercept,
		ActiveAdmissionSlope:     slope,
	})
}

func newRuntimeTraceTuner() *runtimeTraceTuner {
	admissionIntercept, admissionSlope := runtimeAdmissionModelFromEnv()
	t := &runtimeTraceTuner{
		futureUtilModel:       adaptmodel.NewLinearRLS(0, 1, 0.99, 32),
		futureUtilResidualP90: adaptmodel.NewP2Quantile(0.90),
		admissionShareModel:   adaptmodel.NewLinearRLS(admissionIntercept, admissionSlope, 0.99, 16),
	}
	t.snapshot.Store(runtimeTraceSnapshot{
		futureUtilSlope:    1,
		admissionIntercept: admissionIntercept,
		admissionSlope:     admissionSlope,
	})
	return t
}

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

func CurrentRuntimeModelStatus() RuntimeModelStatus {
	snapshot := adaptiveTraceTunerState.Snapshot()
	autoTune := currentAdmissionAutoTuneState()
	return RuntimeModelStatus{
		FutureUtilIntercept:              snapshot.futureUtilIntercept,
		FutureUtilSlope:                  snapshot.futureUtilSlope,
		FutureUtilResidualP90:            snapshot.futureUtilResidualP90,
		FutureUtilSamples:                snapshot.futureUtilSamples,
		AdmissionIntercept:               snapshot.admissionIntercept,
		AdmissionSlope:                   snapshot.admissionSlope,
		AdmissionSamples:                 snapshot.admissionSamples,
		PredictedUtilMilli:               clampNonNegativeInt64ToInt(adaptivePredictedUtilMilli.Load()),
		LearnedAdmissionShareMilli:       clampNonNegativeInt64ToInt(adaptiveAdmissionShareMilli.Load()),
		AutoTuneEnabled:                  autoTune.Enabled,
		AutoTuneIntervalSeconds:          autoTune.IntervalSeconds,
		AutoTuneMinTraceSamples:          autoTune.MinTraceSamples,
		AutoTuneTraceSamples:             autoTune.TraceSamples,
		AutoTuneObservationSamples:       autoTune.ObservationSamples,
		AutoTuneTrainImprovementPct:      autoTune.TrainImprovementPct,
		AutoTuneValidationImprovementPct: autoTune.ValidationImprovementPct,
		AutoTuneRiskIncreasePct:          autoTune.RiskIncreasePct,
		AutoTuneRecommended:              autoTune.Recommended,
		AutoTuneReason:                   autoTune.Reason,
		AutoTuneLastRunAt:                autoTune.LastRunAt,
		AutoTuneLastPromotedAt:           autoTune.LastPromotedAt,
		ActiveAdmissionIntercept:         autoTune.ActiveAdmissionIntercept,
		ActiveAdmissionSlope:             autoTune.ActiveAdmissionSlope,
	}
}

func (t *runtimeTraceTuner) ResetAdmissionModel(intercept, slope float64) runtimeTraceSnapshot {
	if t == nil {
		return runtimeTraceSnapshot{}
	}
	intercept = clampFloat64(intercept, 0.16, 0.38)
	slope = clampFloat64(slope, -0.30, 0.08)

	t.mu.Lock()
	defer t.mu.Unlock()

	t.admissionShareModel = adaptmodel.NewLinearRLS(intercept, slope, 0.99, 16)
	future := t.futureUtilModel.Snapshot()
	snap := runtimeTraceSnapshot{
		futureUtilIntercept:   future.Intercept,
		futureUtilSlope:       future.Slope,
		futureUtilResidualP90: t.futureUtilResidualP90.Estimate(0),
		futureUtilSamples:     future.Samples,
		admissionIntercept:    intercept,
		admissionSlope:        slope,
		admissionSamples:      0,
	}
	t.snapshot.Store(snap)
	return snap
}

func ReportAdmissionAutoTune(report AdmissionAutoTuneReport) {
	adaptiveAutoTuneMu.Lock()
	defer adaptiveAutoTuneMu.Unlock()

	state := currentAdmissionAutoTuneState()
	state.Enabled = report.Enabled
	if report.Interval > 0 {
		state.IntervalSeconds = int64(report.Interval / time.Second)
	}
	if report.MinTraceSamples > 0 {
		state.MinTraceSamples = report.MinTraceSamples
	}
	state.TraceSamples = maxInt(report.TraceSamples, 0)
	state.ObservationSamples = maxInt(report.ObservationSamples, 0)
	state.TrainImprovementPct = report.TrainImprovementPct
	state.ValidationImprovementPct = report.ValidationImprovementPct
	state.RiskIncreasePct = report.RiskIncreasePct
	state.Recommended = report.Recommended
	if strings.TrimSpace(report.Reason) != "" {
		state.Reason = strings.TrimSpace(report.Reason)
	}
	state.LastRunAt = time.Now().UTC()

	if report.Promote {
		intercept := clampFloat64(report.CandidateIntercept, 0.16, 0.38)
		slope := clampFloat64(report.CandidateSlope, -0.30, 0.08)
		snap := adaptiveTraceTunerState.ResetAdmissionModel(intercept, slope)
		state.ActiveAdmissionIntercept = intercept
		state.ActiveAdmissionSlope = slope
		state.LastPromotedAt = state.LastRunAt

		status := CurrentMemoryStatus()
		pressure := clampFloat64(RuntimePressureIndex(status)/3, 0, 1)
		applyAdaptiveTraceStatus(snap, predictiveUtilizationRatio(status), learnedAdmissionShare(status, pressure))
	}

	adaptiveAutoTuneSnapshot.Store(state)
}

func currentAdmissionAutoTuneState() admissionAutoTuneState {
	if state, ok := adaptiveAutoTuneSnapshot.Load().(admissionAutoTuneState); ok {
		return state
	}
	intercept, slope := runtimeAdmissionModelFromEnv()
	return admissionAutoTuneState{
		Reason:                   "boot",
		ActiveAdmissionIntercept: intercept,
		ActiveAdmissionSlope:     slope,
	}
}

func (t *runtimeTraceTuner) Observe(status MemoryStatus, pressureNorm float64) runtimeTraceSnapshot {
	if t == nil {
		return runtimeTraceSnapshot{}
	}
	current := runtimeTraceSample{
		utilization:         currentUtilizationRatio(status),
		baseProjectedUtil:   baseProjectedUtilizationRatio(status),
		pressureNorm:        clampFloat64(pressureNorm, 0, 1),
		headroomBytes:       maxInt64Value(status.MemoryBudgetBytes-status.EffectiveUsedBytes, 0),
		bufferedInUseBytes:  status.BufferedAdmissionInUseBytes,
		bufferedLimitBytes:  status.BufferedAdmissionLimitBytes,
		cgroupHighEvents:    status.CgroupHighEvents,
		cgroupMaxEvents:     status.CgroupMaxEvents,
		cgroupOOMEvents:     status.CgroupOOMEvents,
		cgroupOOMKillEvents: status.CgroupOOMKillEvents,
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.hasPrev {
		predBefore := t.futureUtilModel.Predict(t.prev.baseProjectedUtil)
		t.futureUtilModel.Observe(t.prev.baseProjectedUtil, current.utilization)
		t.futureUtilResidualP90.Observe(maxFloat64(current.utilization-predBefore, 0))
		if target, ok := deriveObservedAdmissionShareTarget(t.prev, current); ok {
			t.admissionShareModel.Observe(t.prev.pressureNorm, target)
		}
	}

	t.prev = current
	t.hasPrev = true

	snap := runtimeTraceSnapshot{
		futureUtilIntercept:   t.futureUtilModel.Snapshot().Intercept,
		futureUtilSlope:       t.futureUtilModel.Snapshot().Slope,
		futureUtilResidualP90: t.futureUtilResidualP90.Estimate(0),
		futureUtilSamples:     t.futureUtilModel.Snapshot().Samples,
		admissionIntercept:    t.admissionShareModel.Snapshot().Intercept,
		admissionSlope:        t.admissionShareModel.Snapshot().Slope,
		admissionSamples:      t.admissionShareModel.Snapshot().Samples,
	}
	t.snapshot.Store(snap)
	return snap
}

func (t *runtimeTraceTuner) Snapshot() runtimeTraceSnapshot {
	if t == nil {
		return runtimeTraceSnapshot{}
	}
	if snap, ok := t.snapshot.Load().(runtimeTraceSnapshot); ok {
		return snap
	}
	return runtimeTraceSnapshot{}
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
	maxGC := deriveAdaptiveMaxGCPercent(baseGC)

	adaptiveGCBase.Store(int64(baseGC))
	adaptiveGCMin.Store(int64(minGC))
	adaptiveGCMax.Store(int64(maxGC))
	adaptiveGCCurrent.Store(int64(baseGC))
	adaptiveGCLastSet.Store(time.Now().Unix())

	if !enabled {
		adaptiveGCEnabled.Store(false)
		adaptiveGCMode.Store(governorModeOff)
		return
	}

	adaptiveGCEnabled.Store(true)
	adaptiveGCMode.Store(governorModeBalanced)
	debug.SetGCPercent(baseGC)

	go runAdaptiveGCController(ctx, adaptiveGCState{currentGCPercent: baseGC})
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
	if status.CgroupHighEvents > 0 {
		pressure += clampFloat64(0.03*float64(status.CgroupHighEvents), 0, 0.25)
	}
	if status.CgroupMaxEvents > 0 {
		pressure += clampFloat64(0.05*float64(status.CgroupMaxEvents), 0, 0.35)
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

func bufferedAdmissionLimitFromStatus(status MemoryStatus) (int64, bool) {
	if status.MemoryBudgetBytes <= 0 || status.EffectiveUsedBytes < 0 || status.MemoryBudgetBytes <= status.EffectiveUsedBytes {
		return 0, false
	}

	headroomBytes := status.MemoryBudgetBytes - status.EffectiveUsedBytes
	if headroomBytes <= 0 {
		return 0, true
	}

	pressure := clampFloat64(RuntimePressureIndex(status)/3, 0, 1)
	share := learnedAdmissionShare(status, pressure)
	limitBytes := clampFloat64ToInt64(float64(headroomBytes) * clampFloat64(share, 0.18, 0.34))
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
			pressure := sampleAdaptiveMemoryPressure(memory, &state)
			state.pressure = smoothAdaptivePressure(state.pressure, pressure)
			traceSnapshot := adaptiveTraceTunerState.Observe(memory, state.pressure)
			predictedUtilization := predictiveUtilizationRatio(memory)
			admissionShare := learnedAdmissionShare(memory, clampFloat64(RuntimePressureIndex(memory)/3, 0, 1))
			applyAdaptiveTraceStatus(traceSnapshot, predictedUtilization, admissionShare)
			targetGC, mode := deriveAdaptiveGCPercent(state.pressure, clampPositiveInt64ToInt(adaptiveGCBase.Load()), clampPositiveInt64ToInt(adaptiveGCMin.Load()), clampPositiveInt64ToInt(adaptiveGCMax.Load()))

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
			appendRuntimeTrace(memory, targetGC, mode, pressureMilli, predictedUtilization, admissionShare)
		}
	}
}

func sampleAdaptiveMemoryPressure(memory MemoryStatus, state *adaptiveGCState) float64 {
	if state == nil {
		return 0
	}

	highDelta := positiveDelta(memory.CgroupHighEvents, state.lastHighEvents)
	maxDelta := positiveDelta(memory.CgroupMaxEvents, state.lastMaxEvents)
	oomDelta := positiveDelta(memory.CgroupOOMEvents, state.lastOOMEvents)
	oomKillDelta := positiveDelta(memory.CgroupOOMKillEvents, state.lastOOMKillEvents)

	state.lastHighEvents = memory.CgroupHighEvents
	state.lastMaxEvents = memory.CgroupMaxEvents
	state.lastOOMEvents = memory.CgroupOOMEvents
	state.lastOOMKillEvents = memory.CgroupOOMKillEvents

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

func deriveAdaptiveGCPercent(pressure float64, baseGC, minGC, maxGC int) (int, int64) {
	if baseGC <= 0 {
		baseGC = defaultAdaptiveGCPercent
	}
	if minGC <= 0 {
		minGC = deriveAdaptiveMinGCPercent(baseGC)
	}
	if maxGC < baseGC {
		maxGC = deriveAdaptiveMaxGCPercent(baseGC)
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

	target := baseGC
	switch {
	case p <= 0.45:
		factor := 0.0
		if p > 0 {
			factor = math.Pow(p/0.45, 1.6)
		}
		target = maxGC - int(math.Round(float64(maxGC-baseGC)*factor))
	default:
		factor := math.Pow((p-0.45)/0.55, 1.2)
		target = baseGC - int(math.Round(float64(baseGC-minGC)*factor))
	}
	return alignGCPercent(clampInt(target, minGC, maxGC)), int64(mode)
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

func applyAdaptiveTraceStatus(snapshot runtimeTraceSnapshot, predictedUtilization, admissionShare float64) {
	adaptiveTraceSamples.Store(maxInt64Value(snapshot.futureUtilSamples, snapshot.admissionSamples))
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

func predictiveUtilizationRatio(status MemoryStatus) float64 {
	utilization := currentUtilizationRatio(status)
	base := baseProjectedUtilizationRatio(status)
	snapshot := adaptiveTraceTunerState.Snapshot()
	if snapshot.futureUtilSamples < 8 {
		return base
	}
	predicted := snapshot.futureUtilIntercept + snapshot.futureUtilSlope*base + snapshot.futureUtilResidualP90
	return clampFloat64(maxFloat64(predicted, utilization), utilization, 1.5)
}

func learnedAdmissionShare(status MemoryStatus, pressureNorm float64) float64 {
	pressureNorm = clampFloat64(pressureNorm, 0, 1)
	defaultShare := clampFloat64(0.34-0.14*pressureNorm, 0.18, 0.34)
	snapshot := adaptiveTraceTunerState.Snapshot()
	if snapshot.admissionSamples < 8 {
		return defaultShare
	}
	share := snapshot.admissionIntercept + snapshot.admissionSlope*pressureNorm
	return clampFloat64(share, 0.16, 0.38)
}

func deriveObservedAdmissionShareTarget(prev, current runtimeTraceSample) (float64, bool) {
	if prev.headroomBytes <= 0 {
		return 0, false
	}
	loadShare := 0.0
	if prev.bufferedInUseBytes > 0 {
		loadShare = float64(prev.bufferedInUseBytes) / float64(prev.headroomBytes)
	}
	if loadShare <= 0 && prev.bufferedLimitBytes <= 0 {
		return 0, false
	}

	target := clampFloat64(maxFloat64(loadShare*1.08, 0.18), 0.16, 0.38)
	highDelta := positiveDelta(current.cgroupHighEvents, prev.cgroupHighEvents)
	maxDelta := positiveDelta(current.cgroupMaxEvents, prev.cgroupMaxEvents)
	oomDelta := positiveDelta(current.cgroupOOMEvents, prev.cgroupOOMEvents)
	oomKillDelta := positiveDelta(current.cgroupOOMKillEvents, prev.cgroupOOMKillEvents)

	switch {
	case oomDelta > 0 || oomKillDelta > 0 || current.utilization >= 0.94:
		target *= 0.68
	case maxDelta > 0 || current.utilization >= 0.90:
		target *= 0.78
	case highDelta > 0 || current.utilization >= 0.84:
		target *= 0.90
	case loadShare > 0.03 && current.utilization <= 0.75:
		target *= 1.05
	}
	return clampFloat64(target, 0.16, 0.38), true
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

func deriveAdaptiveMaxGCPercent(baseGC int) int {
	return clampInt(maxInt(baseGC+20, 100), maxInt(baseGC, 90), 140)
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

func runtimeAdmissionModelFromEnv() (float64, float64) {
	intercept := defaultAdmissionIntercept
	slope := defaultAdmissionSlope

	if value, ok := runtimeFloat64FromEnv("HAZUKI_REWRITE_ADMISSION_INTERCEPT"); ok {
		intercept = clampFloat64(value, 0.16, 0.38)
	}
	if value, ok := runtimeFloat64FromEnv("HAZUKI_REWRITE_ADMISSION_SLOPE"); ok {
		slope = clampFloat64(value, -0.30, 0.08)
	}
	return intercept, slope
}

func runtimeFloat64FromEnv(key string) (float64, bool) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return 0, false
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
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
