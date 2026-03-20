package torcherinoproxy

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hazuki-go/internal/proxy/rewritebudget"
)

const (
	defaultHTMLBufferedRewriteBytes       = 768 << 10
	defaultJSONBufferedRewriteBytes       = 512 << 10
	minBufferedRewriteBytes         int64 = 128 << 10
	maxHTMLBufferedRewriteBytes     int64 = 2 << 20
	maxJSONBufferedRewriteBytes     int64 = 1 << 20
	minStreamRewriteChunkBytes            = 32 << 10
	maxStreamRewriteChunkBytes            = 128 << 10
	streamWorkspaceDivisor          int64 = 12
	streamWorkspaceChunkFactor      int64 = 2
	maxStreamWorkspaceBytes         int64 = 320 << 10
	maxObservedBodyReserveFactor          = 12.0
	rewriteConfidenceTargetSamples        = 24.0
	rewriteHysteresisMargin               = 0.08
	maxInt64                        int64 = int64(^uint64(0) >> 1)
	streamRewriteTailBytes                = 1024
)

type bodyRewriteKind string

const (
	rewriteKindNone bodyRewriteKind = ""
	rewriteKindHTML bodyRewriteKind = "html"
	rewriteKindJSON bodyRewriteKind = "json"
)

type bodyRewritePlan struct {
	Buffered         bool
	BufferedLimit    int64
	StreamChunkBytes int
}

type bodyRewriteConfig struct {
	DefaultBufferedBytes     int64
	MaxBufferedBytes         int64
	BufferedFixedCostBytes   int64
	StreamFixedCostBytes     int64
	DefaultBufferedCost      float64
	MinBufferedCost          float64
	MaxBufferedCost          float64
	StructuralCostBase       float64
	DefaultExpansionRatio    float64
	DefaultPeakP95           float64
	DefaultPeakP99           float64
	DefaultBufferedNsPerByte float64
	DefaultStreamNsPerByte   float64
	DefaultUsableShare       float64
	MinUsableShare           float64
	MaxUsableShare           float64
}

type bodyRewriteTuningSnapshot struct {
	ExpansionRatio   float64
	PeakP95          float64
	PeakP99          float64
	BufferedNs       float64
	StreamNs         float64
	KnownMeanBytes   float64
	KnownP90Bytes    float64
	BufferedSamples  int64
	StreamingSamples int64
}

type RewriteRuntimeStatus struct {
	BudgetSource        string
	MemoryBudgetBytes   int64
	GoMemoryUsedBytes   int64
	EffectiveUsedBytes  int64
	CgroupCurrentBytes  int64
	CgroupHighEvents    int64
	CgroupMaxEvents     int64
	CgroupOOMEvents     int64
	CgroupOOMKillEvents int64
	ActiveRewrites      int64
	HTML                RewriteKindStatus
	JSON                RewriteKindStatus
}

type RewriteKindStatus struct {
	ActiveRewrites                 int64
	RewriteReserveBytes            int64
	HeadroomBytes                  int64
	BufferedLimitBytes             int64
	StreamChunkBytes               int
	UnknownLengthStreams           bool
	BufferedCostMultiplierMilli    int
	UsableShareMilli               int
	RecentBodyP90Bytes             int64
	BufferedSamples                int64
	StreamingSamples               int64
	BufferedThroughputBytesPerSec  int64
	StreamingThroughputBytesPerSec int64
}

type bodyRewriteTunerState struct {
	ExpansionRatio       float64
	PeakP95              float64
	PeakP99              float64
	BufferedNs           float64
	StreamNs             float64
	KnownMeanBytes       float64
	KnownP90Bytes        float64
	BufferedSamples      int64
	StreamingSamples     int64
	DecisionSeenMask     uint32
	DecisionBufferedMask uint32
}

type bodyRewriteRuntimeTuner struct {
	cfg   bodyRewriteConfig
	mu    sync.Mutex
	state bodyRewriteTunerState
}

type limitedCaptureBuffer struct {
	limit     int
	buf       []byte
	truncated bool
}

type countingReader struct {
	src io.Reader
	n   int64
}

var (
	htmlRewriteTuner = newBodyRewriteRuntimeTuner(bodyRewriteConfig{
		DefaultBufferedBytes:     defaultHTMLBufferedRewriteBytes,
		MaxBufferedBytes:         maxHTMLBufferedRewriteBytes,
		BufferedFixedCostBytes:   256 << 10,
		StreamFixedCostBytes:     64 << 10,
		DefaultBufferedCost:      3.15,
		MinBufferedCost:          2.45,
		MaxBufferedCost:          4.40,
		StructuralCostBase:       2.05,
		DefaultExpansionRatio:    1.00,
		DefaultPeakP95:           3.20,
		DefaultPeakP99:           3.55,
		DefaultBufferedNsPerByte: 1.20,
		DefaultStreamNsPerByte:   1.45,
		DefaultUsableShare:       0.68,
		MinUsableShare:           0.56,
		MaxUsableShare:           0.80,
	})
	jsonRewriteTuner = newBodyRewriteRuntimeTuner(bodyRewriteConfig{
		DefaultBufferedBytes:     defaultJSONBufferedRewriteBytes,
		MaxBufferedBytes:         maxJSONBufferedRewriteBytes,
		BufferedFixedCostBytes:   192 << 10,
		StreamFixedCostBytes:     48 << 10,
		DefaultBufferedCost:      2.85,
		MinBufferedCost:          2.10,
		MaxBufferedCost:          4.10,
		StructuralCostBase:       1.90,
		DefaultExpansionRatio:    1.00,
		DefaultPeakP95:           2.95,
		DefaultPeakP99:           3.25,
		DefaultBufferedNsPerByte: 1.05,
		DefaultStreamNsPerByte:   1.30,
		DefaultUsableShare:       0.70,
		MinUsableShare:           0.58,
		MaxUsableShare:           0.82,
	})
	torcherinoRewritePlanner = chooseTorcherinoRewritePlan
	activeHTMLRewriteCount   atomic.Int64
	activeJSONRewriteCount   atomic.Int64
)

func newBodyRewriteRuntimeTuner(cfg bodyRewriteConfig) *bodyRewriteRuntimeTuner {
	return &bodyRewriteRuntimeTuner{
		cfg: cfg,
		state: bodyRewriteTunerState{
			ExpansionRatio: cfg.DefaultExpansionRatio,
			PeakP95:        cfg.DefaultPeakP95,
			PeakP99:        cfg.DefaultPeakP99,
			BufferedNs:     cfg.DefaultBufferedNsPerByte,
			StreamNs:       cfg.DefaultStreamNsPerByte,
		},
	}
}

func (t *bodyRewriteRuntimeTuner) snapshot() bodyRewriteTuningSnapshot {
	if t == nil {
		return bodyRewriteTuningSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return bodyRewriteTuningSnapshot{
		ExpansionRatio:   positiveOrDefaultFloat64(t.state.ExpansionRatio, t.cfg.DefaultExpansionRatio),
		PeakP95:          positiveOrDefaultFloat64(t.state.PeakP95, t.cfg.DefaultPeakP95),
		PeakP99:          positiveOrDefaultFloat64(t.state.PeakP99, t.cfg.DefaultPeakP99),
		BufferedNs:       positiveOrDefaultFloat64(t.state.BufferedNs, t.cfg.DefaultBufferedNsPerByte),
		StreamNs:         positiveOrDefaultFloat64(t.state.StreamNs, t.cfg.DefaultStreamNsPerByte),
		KnownMeanBytes:   maxFloat64(t.state.KnownMeanBytes, 0),
		KnownP90Bytes:    maxFloat64(t.state.KnownP90Bytes, 0),
		BufferedSamples:  t.state.BufferedSamples,
		StreamingSamples: t.state.StreamingSamples,
	}
}

func (t *bodyRewriteRuntimeTuner) choosePlan(memory rewritebudget.MemoryStatus, contentLength int64) bodyRewritePlan {
	active := rewritebudget.CurrentActiveCount()
	if active < 1 {
		active = 1
	}
	snapshot := t.snapshot()
	plan := deriveBodyRewritePlan(t.cfg, memory, contentLength, active, snapshot)
	return t.applyDecisionHysteresis(plan, contentLength)
}

func (t *bodyRewriteRuntimeTuner) applyDecisionHysteresis(plan bodyRewritePlan, contentLength int64) bodyRewritePlan {
	if t == nil || contentLength < 0 || plan.BufferedLimit <= 0 {
		return plan
	}
	bucket := rewriteDecisionBucket(contentLength)
	enterLimit := clampInt64(clampFloat64ToInt64(float64(plan.BufferedLimit)*(1.0-rewriteHysteresisMargin)), minBufferedRewriteBytes, t.cfg.MaxBufferedBytes)
	exitLimit := clampInt64(clampFloat64ToInt64(float64(plan.BufferedLimit)*(1.0+rewriteHysteresisMargin)), minBufferedRewriteBytes, t.cfg.MaxBufferedBytes)

	t.mu.Lock()
	defer t.mu.Unlock()

	mask := uint32(1 << bucket)
	seen := (t.state.DecisionSeenMask & mask) != 0
	stickyBuffered := (t.state.DecisionBufferedMask & mask) != 0

	switch {
	case !seen:
		stickyBuffered = plan.Buffered
	case stickyBuffered:
		if contentLength <= exitLimit {
			plan.Buffered = true
		}
		stickyBuffered = plan.Buffered
	default:
		if contentLength >= enterLimit {
			plan.Buffered = false
		}
		if contentLength < enterLimit {
			stickyBuffered = plan.Buffered
		}
	}

	t.state.DecisionSeenMask |= mask
	if stickyBuffered {
		t.state.DecisionBufferedMask |= mask
	} else {
		t.state.DecisionBufferedMask &^= mask
	}
	return plan
}

func (t *bodyRewriteRuntimeTuner) observeBuffered(inputBytes, outputBytes int64, duration time.Duration, peakExtraBytes int64) {
	if t == nil || inputBytes <= 0 {
		return
	}
	alpha := rewriteSampleAlpha(inputBytes)
	t.mu.Lock()
	defer t.mu.Unlock()

	t.state.BufferedSamples++
	t.observeKnownBodyLocked(float64(inputBytes), alpha)

	if outputBytes > 0 {
		t.state.ExpansionRatio = ewmaFloat64(
			positiveOrDefaultFloat64(t.state.ExpansionRatio, t.cfg.DefaultExpansionRatio),
			float64(outputBytes)/float64(inputBytes),
			alpha,
		)
	}
	if duration > 0 {
		t.state.BufferedNs = ewmaFloat64(
			positiveOrDefaultFloat64(t.state.BufferedNs, t.cfg.DefaultBufferedNsPerByte),
			float64(duration.Nanoseconds())/float64(inputBytes),
			alpha,
		)
	}
	if peakExtraBytes > 0 {
		peakMultiplier := float64(peakExtraBytes) / float64(inputBytes)
		t.state.PeakP95 = updateUpperTailEstimate(
			positiveOrDefaultFloat64(t.state.PeakP95, t.cfg.DefaultPeakP95),
			peakMultiplier,
			alpha,
			0.95,
		)
		t.state.PeakP99 = updateUpperTailEstimate(
			positiveOrDefaultFloat64(t.state.PeakP99, t.cfg.DefaultPeakP99),
			peakMultiplier,
			alpha*0.75,
			0.99,
		)
	}
}

func (t *bodyRewriteRuntimeTuner) observeStreaming(inputBytes int64, duration time.Duration) {
	if t == nil || inputBytes <= 0 {
		return
	}
	alpha := rewriteSampleAlpha(inputBytes)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.StreamingSamples++
	t.observeKnownBodyLocked(float64(inputBytes), alpha)
	if duration > 0 {
		t.state.StreamNs = ewmaFloat64(
			positiveOrDefaultFloat64(t.state.StreamNs, t.cfg.DefaultStreamNsPerByte),
			float64(duration.Nanoseconds())/float64(inputBytes),
			alpha,
		)
	}
}

func (t *bodyRewriteRuntimeTuner) observeKnownBodyLocked(sampleBytes, alpha float64) {
	if sampleBytes <= 0 {
		return
	}
	if t.state.KnownMeanBytes <= 0 {
		t.state.KnownMeanBytes = sampleBytes
	} else {
		t.state.KnownMeanBytes = ewmaFloat64(t.state.KnownMeanBytes, sampleBytes, alpha)
	}
	t.state.KnownP90Bytes = updateUpperTailEstimate(t.state.KnownP90Bytes, sampleBytes, alpha, 0.90)
}

func chooseTorcherinoRewritePlan(kind bodyRewriteKind, contentLength int64) bodyRewritePlan {
	memory := rewritebudget.CurrentMemoryStatus()
	switch kind {
	case rewriteKindHTML:
		return htmlRewriteTuner.choosePlan(memory, contentLength)
	case rewriteKindJSON:
		return jsonRewriteTuner.choosePlan(memory, contentLength)
	default:
		return bodyRewritePlan{StreamChunkBytes: maxStreamRewriteChunkBytes}
	}
}

func currentRewriteTuner(kind bodyRewriteKind) *bodyRewriteRuntimeTuner {
	switch kind {
	case rewriteKindHTML:
		return htmlRewriteTuner
	case rewriteKindJSON:
		return jsonRewriteTuner
	default:
		return nil
	}
}

func CurrentRewriteStatus() RewriteRuntimeStatus {
	memory := rewritebudget.CurrentMemoryStatus()
	totalActiveRewrites := rewritebudget.CurrentActiveCount()
	activeForPlan := maxInt64Value(totalActiveRewrites, 1)
	return RewriteRuntimeStatus{
		BudgetSource:        memory.BudgetSource,
		MemoryBudgetBytes:   memory.MemoryBudgetBytes,
		GoMemoryUsedBytes:   memory.GoMemoryUsedBytes,
		EffectiveUsedBytes:  memory.EffectiveUsedBytes,
		CgroupCurrentBytes:  memory.CgroupCurrentBytes,
		CgroupHighEvents:    memory.CgroupHighEvents,
		CgroupMaxEvents:     memory.CgroupMaxEvents,
		CgroupOOMEvents:     memory.CgroupOOMEvents,
		CgroupOOMKillEvents: memory.CgroupOOMKillEvents,
		ActiveRewrites:      totalActiveRewrites,
		HTML:                currentKindRewriteStatus(memory, activeForPlan, maxInt64Value(activeHTMLRewriteCount.Load(), 0), htmlRewriteTuner),
		JSON:                currentKindRewriteStatus(memory, activeForPlan, maxInt64Value(activeJSONRewriteCount.Load(), 0), jsonRewriteTuner),
	}
}

func currentKindRewriteStatus(memory rewritebudget.MemoryStatus, totalActiveRewrites, kindActiveRewrites int64, tuner *bodyRewriteRuntimeTuner) RewriteKindStatus {
	if tuner == nil {
		return RewriteKindStatus{}
	}

	snapshot := tuner.snapshot()
	plan := deriveBodyRewritePlan(tuner.cfg, memory, -1, totalActiveRewrites, snapshot)

	reserveBytes := int64(0)
	headroomBytes := int64(0)
	usableShare := tuner.cfg.DefaultUsableShare
	if memory.MemoryBudgetBytes > 0 {
		reserveBytes = computeRewriteReserveBytes(memory.MemoryBudgetBytes, snapshot)
		headroomBytes = memory.MemoryBudgetBytes - memory.EffectiveUsedBytes - reserveBytes
		if headroomBytes < 0 {
			headroomBytes = 0
		}
		usableShare = computeRewriteUsableShare(tuner.cfg, memory, headroomBytes, totalActiveRewrites, snapshot)
	}

	return RewriteKindStatus{
		ActiveRewrites:                 kindActiveRewrites,
		RewriteReserveBytes:            reserveBytes,
		HeadroomBytes:                  headroomBytes,
		BufferedLimitBytes:             plan.BufferedLimit,
		StreamChunkBytes:               plan.StreamChunkBytes,
		UnknownLengthStreams:           true,
		BufferedCostMultiplierMilli:    int(computeBufferedRewriteCostMultiplier(tuner.cfg, snapshot) * 1000),
		UsableShareMilli:               int(usableShare * 1000),
		RecentBodyP90Bytes:             clampFloat64ToInt64(snapshot.KnownP90Bytes),
		BufferedSamples:                snapshot.BufferedSamples,
		StreamingSamples:               snapshot.StreamingSamples,
		BufferedThroughputBytesPerSec:  nsPerByteToBytesPerSecond(snapshot.BufferedNs),
		StreamingThroughputBytesPerSec: nsPerByteToBytesPerSecond(snapshot.StreamNs),
	}
}

func deriveBodyRewritePlan(cfg bodyRewriteConfig, memory rewritebudget.MemoryStatus, contentLength, activeRewrites int64, snapshot bodyRewriteTuningSnapshot) bodyRewritePlan {
	plan := bodyRewritePlan{
		BufferedLimit:    cfg.DefaultBufferedBytes,
		StreamChunkBytes: maxStreamRewriteChunkBytes,
	}
	usableBudget := int64(-1)
	bufferedCostMultiplier := computeBufferedRewriteCostMultiplier(cfg, snapshot)

	if memory.MemoryBudgetBytes > 0 && memory.EffectiveUsedBytes >= 0 && memory.MemoryBudgetBytes > memory.EffectiveUsedBytes {
		reserveBytes := computeRewriteReserveBytes(memory.MemoryBudgetBytes, snapshot)
		headroom := memory.MemoryBudgetBytes - memory.EffectiveUsedBytes - reserveBytes
		if headroom <= 0 {
			plan.BufferedLimit = 0
			plan.StreamChunkBytes = minStreamRewriteChunkBytes
			usableBudget = 0
		} else {
			usableShare := computeRewriteUsableShare(cfg, memory, headroom, activeRewrites, snapshot)
			usableBudget = computeRewriteUsableBudgetBytes(headroom, activeRewrites, usableShare)
			plan.BufferedLimit = clampInt64(maxBufferedContentLengthForBudget(cfg, usableBudget, bufferedCostMultiplier), minBufferedRewriteBytes, cfg.MaxBufferedBytes)
			plan.StreamChunkBytes = deriveStreamRewriteChunkBytes(cfg, usableBudget, snapshot)
		}
	}

	plan.Buffered = contentLength >= 0 && plan.BufferedLimit > 0 && contentLength <= plan.BufferedLimit
	if plan.Buffered && usableBudget >= 0 {
		decisionBudget := clampFloat64ToInt64(float64(usableBudget) * computeBufferedDecisionFraction(snapshot))
		plan.Buffered = estimatedBufferedRewriteCostBytes(cfg, contentLength, bufferedCostMultiplier) <= maxInt64Value(decisionBudget, 0)
	}
	return plan
}

func detectRewriteKind(contentType string) bodyRewriteKind {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case strings.Contains(ct, "text/html"):
		return rewriteKindHTML
	case strings.Contains(ct, "application/json"):
		return rewriteKindJSON
	default:
		return rewriteKindNone
	}
}

func beginRewrite(kind bodyRewriteKind) func() {
	finishGlobal := rewritebudget.Begin()
	switch kind {
	case rewriteKindHTML:
		activeHTMLRewriteCount.Add(1)
		return func() {
			activeHTMLRewriteCount.Add(-1)
			finishGlobal()
		}
	case rewriteKindJSON:
		activeJSONRewriteCount.Add(1)
		return func() {
			activeJSONRewriteCount.Add(-1)
			finishGlobal()
		}
	default:
		return finishGlobal
	}
}

func rewriteResponseBuffered(body []byte, reqOrigin string) string {
	return rewriteBody(string(body), reqOrigin)
}

func streamRewriteBodyWithChunkSize(dst io.Writer, src io.Reader, reqOrigin string, chunkSize int) error {
	if strings.TrimSpace(reqOrigin) == "" {
		_, err := io.Copy(dst, src)
		return err
	}

	buf := make([]byte, clampInt(chunkSize, minStreamRewriteChunkBytes, maxStreamRewriteChunkBytes))
	pending := make([]byte, 0, len(buf)+streamRewriteTailBytes)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			consumed, writeErr := flushStreamingRewrite(dst, pending, reqOrigin, err == io.EOF)
			if writeErr != nil {
				return writeErr
			}
			if consumed > 0 {
				pending = append(pending[:0], pending[consumed:]...)
			}
		}

		if err == io.EOF {
			_, writeErr := flushStreamingRewrite(dst, pending, reqOrigin, true)
			return writeErr
		}
		if err != nil {
			return err
		}
	}
}

func flushStreamingRewrite(dst io.Writer, data []byte, reqOrigin string, final bool) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	var out bytes.Buffer
	i := 0
	for {
		idx := findNextRewriteURLStart(data, i)
		if idx < 0 {
			if final {
				out.Write(data[i:])
				if out.Len() > 0 {
					if _, err := dst.Write(out.Bytes()); err != nil {
						return 0, err
					}
				}
				return len(data), nil
			}

			keepFrom := len(data) - 7
			if keepFrom < i {
				keepFrom = i
			}
			out.Write(data[i:keepFrom])
			if out.Len() > 0 {
				if _, err := dst.Write(out.Bytes()); err != nil {
					return 0, err
				}
			}
			return keepFrom, nil
		}

		tokenEnd := findRewriteURLTokenEnd(data, idx)
		if tokenEnd < 0 {
			if final {
				out.Write(data[i:idx])
				out.WriteString(rewriteURLToken(data[idx:], reqOrigin))
				if out.Len() > 0 {
					if _, err := dst.Write(out.Bytes()); err != nil {
						return 0, err
					}
				}
				return len(data), nil
			}
			out.Write(data[i:idx])
			if out.Len() > 0 {
				if _, err := dst.Write(out.Bytes()); err != nil {
					return 0, err
				}
			}
			return idx, nil
		}

		out.Write(data[i:idx])
		out.WriteString(rewriteURLToken(data[idx:tokenEnd], reqOrigin))
		i = tokenEnd
	}
}

func findNextRewriteURLStart(data []byte, from int) int {
	for i := from; i < len(data); i++ {
		if data[i] != 'h' && data[i] != 'H' {
			continue
		}
		if hasRewriteHTTPScheme(data[i:]) {
			return i
		}
	}
	return -1
}

func hasRewriteHTTPScheme(data []byte) bool {
	if len(data) >= len("https://") && strings.EqualFold(string(data[:len("https://")]), "https://") {
		return true
	}
	if len(data) >= len("http://") && strings.EqualFold(string(data[:len("http://")]), "http://") {
		return true
	}
	return false
}

func findRewriteURLTokenEnd(data []byte, start int) int {
	schemeLen := 0
	switch {
	case len(data[start:]) >= len("https://") && strings.EqualFold(string(data[start:start+len("https://")]), "https://"):
		schemeLen = len("https://")
	case len(data[start:]) >= len("http://") && strings.EqualFold(string(data[start:start+len("http://")]), "http://"):
		schemeLen = len("http://")
	default:
		return -1
	}

	for i := start + schemeLen; i < len(data); i++ {
		switch data[i] {
		case '/', '"', '\'', ' ', '\t', '\r', '\n':
			return i
		}
	}
	return -1
}

func rewriteURLToken(token []byte, reqOrigin string) string {
	lower := strings.ToLower(string(token))
	switch {
	case strings.HasSuffix(lower, ".pages.dev"):
		return reqOrigin
	case strings.HasSuffix(lower, ".hf.space"):
		return reqOrigin
	default:
		return string(token)
	}
}

func newLimitedCaptureBuffer(limit int) *limitedCaptureBuffer {
	if limit < 0 {
		limit = 0
	}
	return &limitedCaptureBuffer{limit: limit, buf: make([]byte, 0, limit)}
}

func (b *limitedCaptureBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	if b.limit <= 0 || b.truncated {
		b.truncated = true
		return len(p), nil
	}
	space := b.limit - len(b.buf)
	if space <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > space {
		b.buf = append(b.buf, p[:space]...)
		b.truncated = true
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *limitedCaptureBuffer) Bytes() ([]byte, bool) {
	if b == nil || b.truncated {
		return nil, false
	}
	out := make([]byte, len(b.buf))
	copy(out, b.buf)
	return out, true
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r == nil || r.src == nil {
		return 0, io.EOF
	}
	n, err := r.src.Read(p)
	r.n += int64(n)
	return n, err
}

func computeRewriteReserveBytes(memoryBudgetBytes int64, snapshot bodyRewriteTuningSnapshot) int64 {
	reserve := memoryBudgetBytes / 8
	if snapshot.KnownP90Bytes > 0 {
		observedReserve := clampFloat64ToInt64(snapshot.KnownP90Bytes * maxObservedBodyReserveFactor)
		if observedReserve > reserve {
			reserve = observedReserve
		}
	}
	if reserve < 32<<20 {
		return 32 << 20
	}
	if reserve > 128<<20 {
		return 128 << 20
	}
	return reserve
}

func computeRewriteUsableBudgetBytes(headroomBytes, activeRewrites int64, usableShare float64) int64 {
	if headroomBytes <= 0 {
		return 0
	}
	if activeRewrites < 1 {
		activeRewrites = 1
	}
	perRewriteBudget := headroomBytes / activeRewrites
	if perRewriteBudget <= 0 {
		return 0
	}
	return clampFloat64ToInt64(float64(perRewriteBudget) * usableShare)
}

func computeRewriteUsableShare(cfg bodyRewriteConfig, memory rewritebudget.MemoryStatus, headroomBytes, activeRewrites int64, snapshot bodyRewriteTuningSnapshot) float64 {
	share := cfg.DefaultUsableShare
	confidence := rewriteCalibrationConfidence(snapshot)

	if memory.MemoryBudgetBytes > 0 {
		headroomRatio := float64(headroomBytes) / float64(memory.MemoryBudgetBytes)
		if headroomRatio < 0.30 {
			share -= (0.30 - headroomRatio) * 0.35
		} else if headroomRatio > 0.55 {
			share += (headroomRatio - 0.55) * 0.08
		}
	}

	if memory.MemoryBudgetBytes > 0 && memory.CgroupCurrentBytes > 0 {
		cgroupRatio := float64(memory.CgroupCurrentBytes) / float64(memory.MemoryBudgetBytes)
		if cgroupRatio > 0.78 {
			share -= clampFloat64((cgroupRatio-0.78)*0.40, 0, 0.10)
		}
		if cgroupRatio > 0.90 {
			share -= clampFloat64((cgroupRatio-0.90)*0.80, 0, 0.08)
		}
	}
	if memory.CgroupHighEvents > 0 {
		share -= clampFloat64(0.01*float64(memory.CgroupHighEvents), 0, 0.04)
	}
	if memory.CgroupMaxEvents > 0 {
		share -= clampFloat64(0.02*float64(memory.CgroupMaxEvents), 0, 0.06)
	}
	if memory.CgroupOOMEvents > 0 || memory.CgroupOOMKillEvents > 0 {
		share -= 0.12
	}

	speedGain := bufferedSpeedGainRatio(snapshot)
	share += confidence * clampFloat64(speedGain*0.18, 0, 0.06)
	if activeRewrites > 4 {
		share -= clampFloat64(float64(activeRewrites-4)*0.01, 0, 0.08)
	}
	return clampFloat64(share, cfg.MinUsableShare, cfg.MaxUsableShare)
}

func computeBufferedRewriteCostMultiplier(cfg bodyRewriteConfig, snapshot bodyRewriteTuningSnapshot) float64 {
	structural := cfg.StructuralCostBase + clampFloat64(snapshot.ExpansionRatio, 0.85, 1.50)
	observed := maxFloat64(snapshot.PeakP95*1.01, snapshot.PeakP99*0.99)
	if snapshot.BufferedSamples < 4 {
		observed = maxFloat64(observed, cfg.DefaultBufferedCost)
	}
	return clampFloat64(maxFloat64(structural, observed), cfg.MinBufferedCost, cfg.MaxBufferedCost)
}

func estimatedBufferedRewriteCostBytes(cfg bodyRewriteConfig, contentLength int64, costMultiplier float64) int64 {
	if contentLength <= 0 {
		return cfg.BufferedFixedCostBytes
	}
	extra := clampFloat64ToInt64(float64(contentLength) * clampFloat64(costMultiplier, cfg.MinBufferedCost, cfg.MaxBufferedCost))
	if extra >= maxInt64-cfg.BufferedFixedCostBytes {
		return maxInt64
	}
	return cfg.BufferedFixedCostBytes + extra
}

func maxBufferedContentLengthForBudget(cfg bodyRewriteConfig, usableBudgetBytes int64, costMultiplier float64) int64 {
	if usableBudgetBytes <= cfg.BufferedFixedCostBytes {
		return 0
	}
	return clampFloat64ToInt64(float64(usableBudgetBytes-cfg.BufferedFixedCostBytes) / clampFloat64(costMultiplier, cfg.MinBufferedCost, cfg.MaxBufferedCost))
}

func computeBufferedDecisionFraction(snapshot bodyRewriteTuningSnapshot) float64 {
	confidence := rewriteCalibrationConfidence(snapshot)
	speedGain := bufferedSpeedGainRatio(snapshot)
	return clampFloat64(0.82+confidence*speedGain*0.45, 0.82, 0.97)
}

func deriveStreamRewriteChunkBytes(cfg bodyRewriteConfig, usableBudgetBytes int64, snapshot bodyRewriteTuningSnapshot) int {
	if usableBudgetBytes <= 0 {
		return minStreamRewriteChunkBytes
	}
	workspaceBudget := usableBudgetBytes / streamWorkspaceDivisor
	if workspaceBudget > maxStreamWorkspaceBytes {
		workspaceBudget = maxStreamWorkspaceBytes
	}
	if workspaceBudget <= cfg.StreamFixedCostBytes {
		return minStreamRewriteChunkBytes
	}
	chunkBudget := (workspaceBudget - cfg.StreamFixedCostBytes) / streamWorkspaceChunkFactor
	baseChunk := clampInt(int(chunkBudget), minStreamRewriteChunkBytes, maxStreamRewriteChunkBytes)
	cpuBias := 1.0 + rewriteCalibrationConfidence(snapshot)*clampFloat64(bufferedSpeedGainRatio(snapshot)*0.30, 0, 0.10)
	return alignStreamChunkBytes(clampInt(int(float64(baseChunk)*cpuBias), minStreamRewriteChunkBytes, maxStreamRewriteChunkBytes))
}

func rewriteSampleAlpha(inputBytes int64) float64 {
	if inputBytes <= 0 {
		return 0.10
	}
	alpha := 0.08 + 0.17*(clampFloat64(float64(inputBytes)/float64(2<<20), 0, 1))
	return clampFloat64(alpha, 0.08, 0.25)
}

func rewriteCalibrationConfidence(snapshot bodyRewriteTuningSnapshot) float64 {
	samples := float64(snapshot.BufferedSamples + snapshot.StreamingSamples)
	if samples <= 0 {
		return 0
	}
	return clampFloat64(samples/rewriteConfidenceTargetSamples, 0, 1)
}

func bufferedSpeedGainRatio(snapshot bodyRewriteTuningSnapshot) float64 {
	if snapshot.StreamNs <= 0 || snapshot.BufferedNs <= 0 || snapshot.StreamNs <= snapshot.BufferedNs {
		return 0
	}
	return clampFloat64((snapshot.StreamNs-snapshot.BufferedNs)/snapshot.StreamNs, 0, 0.50)
}

func updateUpperTailEstimate(prev, sample, alpha, quantile float64) float64 {
	if sample <= 0 {
		return prev
	}
	if prev <= 0 {
		return sample
	}
	if sample >= prev {
		return prev + alpha*quantile*(sample-prev)
	}
	return prev + alpha*(1-quantile)*(sample-prev)
}

func ewmaFloat64(prev, sample, alpha float64) float64 {
	if sample <= 0 {
		return prev
	}
	if prev <= 0 {
		return sample
	}
	if alpha <= 0 {
		return prev
	}
	if alpha >= 1 {
		return sample
	}
	return prev + alpha*(sample-prev)
}

func rewriteDecisionBucket(contentLength int64) int {
	switch {
	case contentLength <= 256<<10:
		return 0
	case contentLength <= 512<<10:
		return 1
	case contentLength <= 1<<20:
		return 2
	case contentLength <= 2<<20:
		return 3
	case contentLength <= 4<<20:
		return 4
	default:
		return 5
	}
}

func alignStreamChunkBytes(value int) int {
	const step = 4 << 10
	if value <= minStreamRewriteChunkBytes {
		return minStreamRewriteChunkBytes
	}
	if value >= maxStreamRewriteChunkBytes {
		return maxStreamRewriteChunkBytes
	}
	return clampInt(((value+(step/2))/step)*step, minStreamRewriteChunkBytes, maxStreamRewriteChunkBytes)
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

func clampInt64(value, minValue, maxValue int64) int64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
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

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func maxInt64Value(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func positiveOrDefaultFloat64(value, fallback float64) float64 {
	if value > 0 {
		return value
	}
	return fallback
}

func positiveDeltaInt64(after, before int64) int64 {
	if after <= before {
		return 0
	}
	return after - before
}

func nsPerByteToBytesPerSecond(nsPerByte float64) int64 {
	if nsPerByte <= 0 {
		return 0
	}
	return clampFloat64ToInt64(1_000_000_000 / nsPerByte)
}
