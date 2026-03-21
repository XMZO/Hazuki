package gitproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hazuki-go/internal/model"
	"hazuki-go/internal/proxy/adaptmodel"
	"hazuki-go/internal/proxy/rewritebudget"
	"hazuki-go/internal/proxy/upstreamhttp"
)

const (
	defaultBufferedRewriteBytes         = 1 << 20   // 1 MiB
	minBufferedRewriteBytes             = 128 << 10 // 128 KiB
	maxBufferedRewriteBytes             = 4 << 20   // 4 MiB
	minStreamRewriteChunkBytes          = 32 << 10  // 32 KiB
	maxStreamRewriteChunkBytes          = 128 << 10 // 128 KiB
	minRewriteReserveBytes              = 32 << 20  // 32 MiB
	maxRewriteReserveBytes              = 128 << 20 // 128 MiB
	bufferedRewriteFixedCostBytes       = 384 << 10 // 384 KiB
	streamRewriteFixedCostBytes         = 64 << 10  // 64 KiB
	maxStreamWorkspaceBytes       int64 = 320 << 10 // 320 KiB
	streamWorkspaceDivisor        int64 = 12
	streamWorkspaceChunkFactor    int64 = 2
	maxInt64                      int64 = int64(^uint64(0) >> 1)

	defaultBufferedRewriteCostMultiplier = 3.5
	minBufferedRewriteCostMultiplier     = 2.75
	maxBufferedRewriteCostMultiplier     = 4.75

	defaultBufferedRewriteExpansionRatio = 1.0
	defaultBufferedRewritePeakP95        = 3.55
	defaultBufferedRewritePeakP99        = 3.95
	defaultBufferedRewriteNsPerByte      = 1.15
	defaultStreamRewriteNsPerByte        = 1.42

	defaultRewriteUsableShare = 0.70
	minRewriteUsableShare     = 0.58
	maxRewriteUsableShare     = 0.82

	htmlRewriteConfidenceTargetSamples = 24.0
	maxObservedHTMLReserveFactor       = 16.0
	htmlRewriteHysteresisMargin        = 0.08
	modelByteUnit                      = float64(1 << 20)
)

var (
	htmlRewritePlanner       = chooseHTMLRewritePlan
	htmlRewriteAutoTuner     = newHTMLRewriteAutoTuner()
	activeHTMLRewriteCount   atomic.Int64
	gitStreamChunkBufferPool = sync.Pool{
		New: func() any {
			return make([]byte, maxStreamRewriteChunkBytes)
		},
	}
)

type CorsOrigins struct {
	Kind      string
	AllowList map[string]struct{}
}

type RuntimeConfig struct {
	Upstream       string
	UpstreamMobile string
	UpstreamPath   string
	HTTPS          bool

	GithubToken      string
	GithubAuthScheme string

	DisableCache      bool
	CacheControl      string
	CacheControlMedia string
	CacheControlText  string

	CorsOrigins          CorsOrigins
	CorsAllowCredentials bool
	CorsExposeHeaders    string

	BlockedRegions     map[string]struct{}
	BlockedIPAddresses map[string]struct{}

	ReplaceDict map[string]string

	Host string
	Port int
}

type htmlRewritePlan struct {
	Buffered           bool
	BufferedLimit      int64
	StreamChunkBytes   int
	EstimatedCostBytes int64
}

type HTMLRewriteStatus struct {
	BudgetSource         string
	MemoryBudgetBytes    int64
	GoMemoryUsedBytes    int64
	EffectiveUsedBytes   int64
	CgroupCurrentBytes   int64
	RewriteReserveBytes  int64
	HeadroomBytes        int64
	BufferedLimitBytes   int64
	StreamChunkBytes     int
	UnknownLengthStreams bool

	ActiveRewrites                 int64
	BufferedCostMultiplierMilli    int
	UsableShareMilli               int
	RecentHTMLP90Bytes             int64
	BufferedSamples                int64
	StreamingSamples               int64
	BufferedThroughputBytesPerSec  int64
	StreamingThroughputBytesPerSec int64
	CgroupHighEvents               int64
	CgroupMaxEvents                int64
	CgroupOOMEvents                int64
	CgroupOOMKillEvents            int64
}

type htmlRewriteTuningSnapshot struct {
	BufferedExpansionRatio        float64
	BufferedPeakP95               float64
	BufferedPeakP99               float64
	BufferedNsPerByte             float64
	StreamNsPerByte               float64
	KnownHTMLMeanBytes            float64
	KnownHTMLP90Bytes             float64
	BufferedSamples               int64
	StreamingSamples              int64
	BufferedPeakIntercept         float64
	BufferedPeakSlope             float64
	BufferedPeakResidualP95       float64
	BufferedPeakResidualP99       float64
	BufferedPeakModelSamples      int64
	BufferedDurationIntercept     float64
	BufferedDurationSlope         float64
	BufferedDurationModelSamples  int64
	StreamingDurationIntercept    float64
	StreamingDurationSlope        float64
	StreamingDurationModelSamples int64
}

type htmlRewriteAutoTunerState struct {
	BufferedExpansionRatio  float64
	BufferedPeakP95         float64
	BufferedPeakP99         float64
	BufferedNsPerByte       float64
	StreamNsPerByte         float64
	KnownHTMLMeanBytes      float64
	KnownHTMLP90Bytes       float64
	BufferedSamples         int64
	StreamingSamples        int64
	DecisionSeenMask        uint32
	DecisionBufferedMask    uint32
	KnownHTMLP90Quantile    adaptmodel.P2Quantile
	BufferedPeakP95Quantile adaptmodel.P2Quantile
	BufferedPeakP99Quantile adaptmodel.P2Quantile
	BufferedPeakModel       adaptmodel.LinearRLS
	BufferedPeakResidualP95 adaptmodel.P2Quantile
	BufferedPeakResidualP99 adaptmodel.P2Quantile
	BufferedDurationModel   adaptmodel.LinearRLS
	StreamingDurationModel  adaptmodel.LinearRLS
}

type htmlRewriteRuntimeTuner struct {
	mu    sync.Mutex
	state htmlRewriteAutoTunerState
}

func newHTMLRewriteAutoTuner() *htmlRewriteRuntimeTuner {
	return &htmlRewriteRuntimeTuner{
		state: htmlRewriteAutoTunerState{
			BufferedExpansionRatio:  defaultBufferedRewriteExpansionRatio,
			BufferedPeakP95:         defaultBufferedRewritePeakP95,
			BufferedPeakP99:         defaultBufferedRewritePeakP99,
			BufferedNsPerByte:       defaultBufferedRewriteNsPerByte,
			StreamNsPerByte:         defaultStreamRewriteNsPerByte,
			KnownHTMLP90Quantile:    adaptmodel.NewP2Quantile(0.90),
			BufferedPeakP95Quantile: adaptmodel.NewP2Quantile(0.95),
			BufferedPeakP99Quantile: adaptmodel.NewP2Quantile(0.99),
			BufferedPeakModel:       adaptmodel.NewLinearRLS(bytesToModelUnits(bufferedRewriteFixedCostBytes), defaultBufferedRewriteCostMultiplier, 0.985, 64),
			BufferedPeakResidualP95: adaptmodel.NewP2Quantile(0.95),
			BufferedPeakResidualP99: adaptmodel.NewP2Quantile(0.99),
			BufferedDurationModel:   adaptmodel.NewLinearRLS(0, defaultBufferedRewriteNsPerByte*modelByteUnit/1e6, 0.985, 32),
			StreamingDurationModel:  adaptmodel.NewLinearRLS(0, defaultStreamRewriteNsPerByte*modelByteUnit/1e6, 0.985, 32),
		},
	}
}

func defaultHTMLRewriteTuningSnapshot() htmlRewriteTuningSnapshot {
	return htmlRewriteTuningSnapshot{
		BufferedExpansionRatio: defaultBufferedRewriteExpansionRatio,
		BufferedPeakP95:        defaultBufferedRewritePeakP95,
		BufferedPeakP99:        defaultBufferedRewritePeakP99,
		BufferedNsPerByte:      defaultBufferedRewriteNsPerByte,
		StreamNsPerByte:        defaultStreamRewriteNsPerByte,
	}
}

func (t *htmlRewriteRuntimeTuner) snapshot() htmlRewriteTuningSnapshot {
	if t == nil {
		return defaultHTMLRewriteTuningSnapshot()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.state
	return htmlRewriteTuningSnapshot{
		BufferedExpansionRatio:        positiveOrDefaultFloat64(s.BufferedExpansionRatio, defaultBufferedRewriteExpansionRatio),
		BufferedPeakP95:               quantileOrDefault(&s.BufferedPeakP95Quantile, positiveOrDefaultFloat64(s.BufferedPeakP95, defaultBufferedRewritePeakP95)),
		BufferedPeakP99:               quantileOrDefault(&s.BufferedPeakP99Quantile, positiveOrDefaultFloat64(s.BufferedPeakP99, defaultBufferedRewritePeakP99)),
		BufferedNsPerByte:             positiveOrDefaultFloat64(s.BufferedNsPerByte, defaultBufferedRewriteNsPerByte),
		StreamNsPerByte:               positiveOrDefaultFloat64(s.StreamNsPerByte, defaultStreamRewriteNsPerByte),
		KnownHTMLMeanBytes:            maxFloat64(s.KnownHTMLMeanBytes, 0),
		KnownHTMLP90Bytes:             maxFloat64(modelUnitsToBytes(quantileOrDefault(&s.KnownHTMLP90Quantile, bytesToModelUnits(clampFloat64ToInt64(s.KnownHTMLP90Bytes)))), 0),
		BufferedSamples:               s.BufferedSamples,
		StreamingSamples:              s.StreamingSamples,
		BufferedPeakIntercept:         modelUnitsToBytes(s.BufferedPeakModel.Snapshot().Intercept),
		BufferedPeakSlope:             s.BufferedPeakModel.Snapshot().Slope,
		BufferedPeakResidualP95:       modelUnitsToBytes(quantileOrDefault(&s.BufferedPeakResidualP95, 0)),
		BufferedPeakResidualP99:       modelUnitsToBytes(quantileOrDefault(&s.BufferedPeakResidualP99, 0)),
		BufferedPeakModelSamples:      s.BufferedPeakModel.Snapshot().Samples,
		BufferedDurationIntercept:     durationModelUnitsToNs(s.BufferedDurationModel.Snapshot().Intercept),
		BufferedDurationSlope:         durationModelSlopeUnitsToNsPerByte(s.BufferedDurationModel.Snapshot().Slope),
		BufferedDurationModelSamples:  s.BufferedDurationModel.Snapshot().Samples,
		StreamingDurationIntercept:    durationModelUnitsToNs(s.StreamingDurationModel.Snapshot().Intercept),
		StreamingDurationSlope:        durationModelSlopeUnitsToNsPerByte(s.StreamingDurationModel.Snapshot().Slope),
		StreamingDurationModelSamples: s.StreamingDurationModel.Snapshot().Samples,
	}
}

func (t *htmlRewriteRuntimeTuner) choosePlan(memory rewritebudget.MemoryStatus, contentLength, activeRewrites int64) htmlRewritePlan {
	snapshot := t.snapshot()
	plan := deriveHTMLRewritePlanWithSnapshot(memory, contentLength, activeRewrites, snapshot)
	if t == nil {
		return plan
	}
	return t.applyDecisionHysteresis(plan, contentLength, snapshot)
}

func (t *htmlRewriteRuntimeTuner) applyDecisionHysteresis(plan htmlRewritePlan, contentLength int64, snapshot htmlRewriteTuningSnapshot) htmlRewritePlan {
	if t == nil || contentLength < 0 || plan.BufferedLimit <= 0 {
		return plan
	}

	bucket := htmlRewriteDecisionBucket(contentLength)
	enterLimit := clampInt64(
		clampFloat64ToInt64(float64(plan.BufferedLimit)*(1.0-htmlRewriteHysteresisMargin)),
		minBufferedRewriteBytes,
		maxBufferedRewriteBytes,
	)
	exitLimit := clampInt64(
		clampFloat64ToInt64(float64(plan.BufferedLimit)*(1.0+htmlRewriteHysteresisMargin)),
		minBufferedRewriteBytes,
		maxBufferedRewriteBytes,
	)

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

func (t *htmlRewriteRuntimeTuner) observeBuffered(inputBytes, outputBytes int64, duration time.Duration, peakExtraBytes int64) {
	if t == nil || inputBytes <= 0 {
		return
	}

	alpha := htmlRewriteSampleAlpha(inputBytes)

	t.mu.Lock()
	defer t.mu.Unlock()

	t.state.BufferedSamples++
	t.observeKnownHTMLLocked(float64(inputBytes), alpha)

	if outputBytes > 0 {
		expansion := float64(outputBytes) / float64(inputBytes)
		t.state.BufferedExpansionRatio = ewmaFloat64(
			positiveOrDefaultFloat64(t.state.BufferedExpansionRatio, defaultBufferedRewriteExpansionRatio),
			expansion,
			alpha,
		)
	}

	if duration > 0 {
		nsPerByte := float64(duration.Nanoseconds()) / float64(inputBytes)
		t.state.BufferedNsPerByte = ewmaFloat64(
			positiveOrDefaultFloat64(t.state.BufferedNsPerByte, defaultBufferedRewriteNsPerByte),
			nsPerByte,
			alpha,
		)
		t.state.BufferedDurationModel.Observe(
			bytesToModelUnits(inputBytes),
			durationToModelUnits(duration),
		)
	}

	if peakExtraBytes > 0 {
		prevPeakModel := t.state.BufferedPeakModel.Snapshot()
		predictedPeak := modelPredictBytes(prevPeakModel, inputBytes)
		overshoot := positiveDeltaFloat64(float64(peakExtraBytes), predictedPeak)
		t.state.BufferedPeakResidualP95.Observe(bytesToModelUnits(clampFloat64ToInt64(overshoot)))
		t.state.BufferedPeakResidualP99.Observe(bytesToModelUnits(clampFloat64ToInt64(overshoot)))
		t.state.BufferedPeakModel.Observe(
			bytesToModelUnits(inputBytes),
			bytesToModelUnits(peakExtraBytes),
		)
		peakMultiplier := float64(peakExtraBytes) / float64(inputBytes)
		t.state.BufferedPeakP95 = updateUpperTailEstimate(
			positiveOrDefaultFloat64(t.state.BufferedPeakP95, defaultBufferedRewritePeakP95),
			peakMultiplier,
			alpha,
			0.95,
		)
		t.state.BufferedPeakP99 = updateUpperTailEstimate(
			positiveOrDefaultFloat64(t.state.BufferedPeakP99, defaultBufferedRewritePeakP99),
			peakMultiplier,
			alpha*0.75,
			0.99,
		)
		t.state.BufferedPeakP95Quantile.Observe(peakMultiplier)
		t.state.BufferedPeakP99Quantile.Observe(peakMultiplier)
	}
}

func (t *htmlRewriteRuntimeTuner) observeStreaming(inputBytes int64, duration time.Duration) {
	if t == nil || inputBytes <= 0 {
		return
	}

	alpha := htmlRewriteSampleAlpha(inputBytes)

	t.mu.Lock()
	defer t.mu.Unlock()

	t.state.StreamingSamples++
	t.observeKnownHTMLLocked(float64(inputBytes), alpha)

	if duration > 0 {
		nsPerByte := float64(duration.Nanoseconds()) / float64(inputBytes)
		t.state.StreamNsPerByte = ewmaFloat64(
			positiveOrDefaultFloat64(t.state.StreamNsPerByte, defaultStreamRewriteNsPerByte),
			nsPerByte,
			alpha,
		)
		t.state.StreamingDurationModel.Observe(
			bytesToModelUnits(inputBytes),
			durationToModelUnits(duration),
		)
	}
}

func (t *htmlRewriteRuntimeTuner) observeKnownHTMLLocked(sampleBytes, alpha float64) {
	if sampleBytes <= 0 {
		return
	}
	if t.state.KnownHTMLMeanBytes <= 0 {
		t.state.KnownHTMLMeanBytes = sampleBytes
	} else {
		t.state.KnownHTMLMeanBytes = ewmaFloat64(t.state.KnownHTMLMeanBytes, sampleBytes, alpha)
	}
	t.state.KnownHTMLP90Quantile.Observe(bytesToModelUnits(clampFloat64ToInt64(sampleBytes)))
	t.state.KnownHTMLP90Bytes = updateUpperTailEstimate(
		t.state.KnownHTMLP90Bytes,
		sampleBytes,
		alpha,
		0.90,
	)
}

func htmlRewriteSampleAlpha(inputBytes int64) float64 {
	if inputBytes <= 0 {
		return 0.10
	}
	alpha := 0.08 + 0.17*(clampFloat64(float64(inputBytes)/float64(2<<20), 0, 1))
	return clampFloat64(alpha, 0.08, 0.25)
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

func BuildRuntimeConfig(cfg model.AppConfig) (RuntimeConfig, error) {
	upstream := strings.TrimSpace(cfg.Git.Upstream)
	if upstream == "" {
		upstream = "raw.githubusercontent.com"
	}
	upstreamMobile := strings.TrimSpace(cfg.Git.UpstreamMobile)
	if upstreamMobile == "" {
		upstreamMobile = upstream
	}

	upstreamPath := normalizeUpstreamPath(cfg.Git.UpstreamPath)
	if upstreamPath == "" && strings.TrimSpace(cfg.Git.UpstreamPath) != "/" {
		// normalizeUpstreamPath("/") becomes "", which is valid (means "no prefix").
		// For other empty-ish values, fail fast.
		return RuntimeConfig{}, errors.New("git.upstreamPath is required")
	}

	replaceDict := cfg.Git.ReplaceDict
	if replaceDict == nil {
		replaceDict = map[string]string{"$upstream": "$custom_domain"}
	}

	blockedRegions := make(map[string]struct{}, len(cfg.Git.BlockedRegions))
	for _, r := range cfg.Git.BlockedRegions {
		rr := strings.ToUpper(strings.TrimSpace(r))
		if rr == "" {
			continue
		}
		blockedRegions[rr] = struct{}{}
	}
	blockedIPs := make(map[string]struct{}, len(cfg.Git.BlockedIpAddresses))
	for _, ip := range cfg.Git.BlockedIpAddresses {
		s := strings.TrimSpace(ip)
		if s == "" {
			continue
		}
		blockedIPs[s] = struct{}{}
	}

	corsOrigins := parseCorsOrigins(cfg.Git.CorsOrigin)
	corsAllowCreds := cfg.Git.CorsAllowCredentials
	if corsAllowCreds && corsOrigins.Kind == "any" {
		// Same as Node: credentials + "*" is invalid; disable credentials.
		corsAllowCreds = false
	}

	port := cfg.Ports.Git
	if port == 0 {
		port = 3002
	}

	return RuntimeConfig{
		Upstream:       upstream,
		UpstreamMobile: upstreamMobile,
		UpstreamPath:   upstreamPath,
		HTTPS:          cfg.Git.HTTPS,

		GithubToken:      cfg.Git.GithubToken,
		GithubAuthScheme: strings.TrimSpace(defaultString(cfg.Git.GithubAuthScheme, "token")),

		DisableCache:      cfg.Git.DisableCache,
		CacheControl:      strings.TrimSpace(cfg.Git.CacheControl),
		CacheControlMedia: strings.TrimSpace(defaultString(cfg.Git.CacheControlMedia, "public, max-age=43200000")),
		CacheControlText:  strings.TrimSpace(defaultString(cfg.Git.CacheControlText, "public, max-age=60")),

		CorsOrigins:          corsOrigins,
		CorsAllowCredentials: corsAllowCreds,
		CorsExposeHeaders:    strings.TrimSpace(cfg.Git.CorsExposeHeaders),

		BlockedRegions:     blockedRegions,
		BlockedIPAddresses: blockedIPs,

		ReplaceDict: replaceDict,

		Host: "0.0.0.0",
		Port: port,
	}, nil
}

func NewHandler(runtime RuntimeConfig) http.Handler {
	return NewDynamicHandler(func() RuntimeConfig { return runtime })
}

func NewDynamicHandler(getRuntime func() RuntimeConfig) http.Handler {
	client := upstreamhttp.NewClient(upstreamhttp.Options{
		FollowRedirects: false,
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime := RuntimeConfig{}
		if getRuntime != nil {
			runtime = getRuntime()
		}
		handleRequest(w, r, runtime, client)
	})
}

func handleRequest(w http.ResponseWriter, r *http.Request, runtime RuntimeConfig, client *http.Client) {
	originalHost := getOriginalHost(r)
	originalProto := getOriginalProto(r)
	requestOrigin := r.Header.Get("Origin")

	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/_hazuki/health" {
		if !isLoopbackRemoteAddr(r.RemoteAddr) {
			http.NotFound(w, r)
			return
		}

		payload := map[string]any{
			"ok":             true,
			"service":        "git",
			"host":           runtime.Host,
			"port":           runtime.Port,
			"upstream":       runtime.Upstream,
			"upstreamMobile": runtime.UpstreamMobile,
			"upstreamPath":   runtime.UpstreamPath,
			"https":          runtime.HTTPS,
			"tokenSet":       runtime.GithubToken != "",
			"disableCache":   runtime.DisableCache,
			"corsOrigin": func() any {
				if runtime.CorsOrigins.Kind == "any" {
					return "*"
				}
				out := make([]string, 0, len(runtime.CorsOrigins.AllowList))
				for v := range runtime.CorsOrigins.AllowList {
					out = append(out, v)
				}
				return out
			}(),
			"time": time.Now().UTC().Format(time.RFC3339Nano),
		}

		buf, _ := json.MarshalIndent(payload, "", "  ")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(buf)
		return
	}

	region := strings.ToUpper(strings.TrimSpace(r.Header.Get("Cf-Ipcountry")))
	clientIP := getClientIP(r)
	userAgent := r.Header.Get("User-Agent")

	if _, ok := runtime.BlockedRegions[region]; ok && region != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Access denied: service is not available in your region yet."))
		return
	}

	if _, ok := runtime.BlockedIPAddresses[clientIP]; ok && clientIP != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Access denied: your IP address is blocked."))
		return
	}

	if r.Method == http.MethodOptions {
		preflightHeaders := buildPreflightResponseHeaders(r, requestOrigin, runtime)
		for k, vals := range preflightHeaders {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	upstreamDomain := runtime.Upstream
	if !isDesktopDevice(userAgent) {
		upstreamDomain = runtime.UpstreamMobile
	}

	upstreamURL := &url.URL{
		Scheme:   "http",
		Host:     upstreamDomain,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}
	if runtime.HTTPS {
		upstreamURL.Scheme = "https"
	}

	// Apply upstream path prefix.
	if upstreamURL.Path == "" || upstreamURL.Path == "/" {
		upstreamURL.Path = runtime.UpstreamPath
	} else {
		upstreamURL.Path = runtime.UpstreamPath + upstreamURL.Path
	}

	var body io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		body = r.Body
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), body)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("Bad gateway"))
		return
	}

	upstreamReq.Header = buildUpstreamRequestHeaders(r, upstreamDomain, originalHost, originalProto, runtime.GithubToken, runtime.GithubAuthScheme)
	upstreamReq.Host = upstreamDomain
	upstreamReq.ContentLength = r.ContentLength

	resp, err := client.Do(upstreamReq)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("Bad gateway"))
		return
	}
	defer resp.Body.Close()

	upstreamContentType := resp.Header.Get("Content-Type")
	effectiveContentType := upstreamContentType
	normalizedCt := strings.ToLower(strings.TrimSpace(upstreamContentType))
	if normalizedCt == "" || strings.HasPrefix(normalizedCt, "application/octet-stream") {
		if guessed := guessMimeFromPathname(r.URL.Path); guessed != "" {
			effectiveContentType = guessed
		}
	}

	// HTML rewrite mode is chosen from a simple memory cost model so small hosts
	// can stream under pressure while larger or quieter hosts keep the cheaper
	// buffered path.
	shouldRewrite := shouldRewriteHTML(effectiveContentType)

	cacheControl := computeCacheControl(runtime.DisableCache, effectiveContentType, runtime.CacheControl, runtime.CacheControlMedia, runtime.CacheControlText)

	clientHeaders := buildClientResponseHeaders(resp.Header, upstreamDomain, originalHost, r.URL.Path, cacheControl, requestOrigin, runtime, shouldRewrite)
	for k, vals := range clientHeaders {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)

	if r.Method == http.MethodHead {
		return
	}

	if shouldRewrite {
		rewritePlan := htmlRewritePlanner(resp.ContentLength)
		var releaseBufferedAdmission func()
		if rewritePlan.Buffered {
			releaseBufferedAdmission, rewritePlan.Buffered = rewritebudget.AcquireBufferedAdmission(rewritePlan.EstimatedCostBytes)
			if !rewritePlan.Buffered {
				rewritePlan.EstimatedCostBytes = estimatedStreamingRewriteCostBytes(rewritePlan.StreamChunkBytes)
			}
		}
		finishRewrite := beginHTMLRewrite(rewritePlan.EstimatedCostBytes)
		defer finishRewrite()
		if releaseBufferedAdmission != nil {
			defer releaseBufferedAdmission()
		}
		if rewritePlan.Buffered {
			startedAt := time.Now()
			beforeUsed := rewritebudget.CurrentMemoryStatus().GoMemoryUsedBytes
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				return
			}
			afterReadUsed := rewritebudget.CurrentMemoryStatus().GoMemoryUsedBytes
			rewritten := applyReplacements(string(raw), upstreamDomain, originalHost, runtime.ReplaceDict)
			afterRewriteUsed := rewritebudget.CurrentMemoryStatus().GoMemoryUsedBytes
			if _, err := io.WriteString(w, rewritten); err == nil {
				htmlRewriteAutoTuner.observeBuffered(
					int64(len(raw)),
					int64(len(rewritten)),
					time.Since(startedAt),
					maxInt64Value(
						positiveDeltaInt64(afterReadUsed, beforeUsed),
						positiveDeltaInt64(afterRewriteUsed, beforeUsed),
					),
				)
			}
			return
		}
		startedAt := time.Now()
		countedBody := &countingReader{src: resp.Body}
		countedWriter := &countingWriter{dst: w}
		if err := streamApplyReplacementsWithChunkSize(countedWriter, countedBody, upstreamDomain, originalHost, runtime.ReplaceDict, rewritePlan.StreamChunkBytes); err == nil {
			htmlRewriteAutoTuner.observeStreaming(countedBody.n, time.Since(startedAt))
		}
		return
	}

	_, _ = io.Copy(w, resp.Body)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func normalizeUpstreamPath(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "/"
	}
	withLeading := trimmed
	if !strings.HasPrefix(withLeading, "/") {
		withLeading = "/" + withLeading
	}
	if strings.HasSuffix(withLeading, "/") {
		return strings.TrimSuffix(withLeading, "/")
	}
	return withLeading
}

func parseCorsOrigins(value string) CorsOrigins {
	raw := strings.TrimSpace(value)
	if raw == "" || raw == "*" {
		return CorsOrigins{Kind: "any"}
	}
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return CorsOrigins{Kind: "list", AllowList: out}
}

func getOriginalHost(r *http.Request) string {
	xfHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if xfHost != "" {
		return xfHost
	}
	if r.Host != "" {
		return r.Host
	}
	return "localhost"
}

func getOriginalProto(r *http.Request) string {
	xfProto := r.Header.Get("X-Forwarded-Proto")
	if xfProto != "" {
		v := strings.TrimSpace(strings.Split(xfProto, ",")[0])
		if v != "" {
			return v
		}
	}
	return "http"
}

func getClientIP(r *http.Request) string {
	if cf := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); cf != "" {
		return cf
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if first != "" {
			return first
		}
	}
	host := strings.TrimSpace(r.RemoteAddr)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func isDesktopDevice(userAgent string) bool {
	agents := []string{"Android", "iPhone", "SymbianOS", "Windows Phone", "iPad", "iPod"}
	for _, a := range agents {
		if strings.Contains(userAgent, a) {
			return false
		}
	}
	return true
}

func buildUpstreamRequestHeaders(r *http.Request, upstreamDomain, originalHost, originalProto, githubToken, githubAuthScheme string) http.Header {
	headers := make(http.Header)
	for key, values := range r.Header {
		lowerKey := strings.ToLower(key)
		if shouldSkipUpstreamHeader(lowerKey) {
			continue
		}
		for _, v := range values {
			headers.Add(lowerKey, v)
		}
	}

	headers.Set("referer", originalProto+"://"+originalHost)
	headers.Set("accept-encoding", "identity")

	if strings.TrimSpace(githubToken) != "" {
		scheme := strings.TrimSpace(githubAuthScheme)
		if scheme == "" {
			scheme = "token"
		}
		headers.Set("authorization", scheme+" "+githubToken)
	}

	// Ensure upstream host is correct.
	headers.Set("host", upstreamDomain)
	return headers
}

func shouldSkipUpstreamHeader(lowerKey string) bool {
	switch lowerKey {
	case "connection",
		"keep-alive",
		"proxy-authenticate",
		"proxy-authorization",
		"authorization",
		"cookie",
		"te",
		"trailer",
		"transfer-encoding",
		"upgrade",
		"host",
		"accept-encoding",
		"content-length":
		return true
	default:
		return false
	}
}

func buildClientResponseHeaders(
	upstreamHeaders http.Header,
	upstreamDomain string,
	originalHost string,
	requestPathname string,
	cacheControl string,
	requestOrigin string,
	runtime RuntimeConfig,
	shouldRewrite bool,
) http.Header {
	headers := make(http.Header)

	for key, values := range upstreamHeaders {
		lowerKey := strings.ToLower(key)
		if shouldSkipClientHeader(lowerKey) {
			continue
		}
		if lowerKey == "content-length" && shouldRewrite {
			continue
		}
		for _, v := range values {
			headers.Add(lowerKey, v)
		}
	}

	maybeFixOctetStreamContentType(headers, requestPathname)

	headers.Set("cache-control", cacheControl)

	applyCorsHeaders(headers, requestOrigin, runtime.CorsOrigins, runtime.CorsAllowCredentials, runtime.CorsExposeHeaders)

	if v := headers.Get("x-pjax-url"); v != "" {
		headers.Set("x-pjax-url", strings.ReplaceAll(v, "//"+upstreamDomain, "//"+originalHost))
	}

	if vary := headers.Get("vary"); vary != "" {
		headers.Set("vary", removeVaryHeaderValue(vary, "authorization"))
	}

	return headers
}

func shouldSkipClientHeader(lowerKey string) bool {
	switch lowerKey {
	case "content-security-policy",
		"content-security-policy-report-only",
		"clear-site-data",
		"content-encoding",
		"connection",
		"keep-alive",
		"proxy-authenticate",
		"proxy-authorization",
		"te",
		"trailer",
		"transfer-encoding",
		"upgrade":
		return true
	default:
		return false
	}
}

func maybeFixOctetStreamContentType(headers http.Header, requestPathname string) {
	ct := strings.ToLower(strings.TrimSpace(headers.Get("content-type")))
	if ct != "" && !strings.HasPrefix(ct, "application/octet-stream") {
		return
	}
	guessed := guessMimeFromPathname(requestPathname)
	if guessed == "" {
		return
	}
	headers.Set("content-type", guessed)
}

func guessMimeFromPathname(pathname string) string {
	base := pathname
	if idx := strings.LastIndex(base, "/"); idx != -1 {
		base = base[idx+1:]
	}
	dot := strings.LastIndex(base, ".")
	if dot == -1 || dot == len(base)-1 {
		return ""
	}
	ext := strings.ToLower(base[dot+1:])

	switch ext {
	case "js", "mjs", "cjs", "jsx":
		return "application/javascript; charset=utf-8"
	case "css":
		return "text/css; charset=utf-8"
	case "html", "htm":
		return "text/html; charset=utf-8"
	case "json", "map":
		return "application/json; charset=utf-8"
	case "yml", "yaml":
		return "application/x-yaml; charset=utf-8"
	case "toml":
		return "application/toml; charset=utf-8"
	case "xml":
		return "application/xml; charset=utf-8"
	case "txt", "md":
		return "text/plain; charset=utf-8"
	case "csv":
		return "text/csv; charset=utf-8"
	case "m3u", "m3u8":
		return "application/vnd.apple.mpegurl; charset=utf-8"
	case "wasm":
		return "application/wasm"
	case "webm":
		return "video/webm"
	case "mp4":
		return "video/mp4"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "ogg":
		return "audio/ogg"
	case "m4a":
		return "audio/mp4"
	case "webp":
		return "image/webp"
	case "avif":
		return "image/avif"
	case "png":
		return "image/png"
	case "ico":
		return "image/x-icon"
	case "cur":
		return "image/x-icon"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "svg":
		return "image/svg+xml"
	case "woff2":
		return "font/woff2"
	case "woff":
		return "font/woff"
	case "ttf":
		return "font/ttf"
	case "otf":
		return "font/otf"
	case "eot":
		return "application/vnd.ms-fontobject"
	default:
		return ""
	}
}

func shouldRewriteHTML(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "text/html") && strings.Contains(ct, "utf-8")
}

func chooseHTMLRewritePlan(contentLength int64) htmlRewritePlan {
	return htmlRewriteAutoTuner.choosePlan(
		rewritebudget.CurrentMemoryStatus(),
		contentLength,
		currentHTMLRewriteConcurrency(),
	)
}

func CurrentHTMLRewriteStatus() HTMLRewriteStatus {
	memory := rewritebudget.CurrentMemoryStatus()
	totalActiveRewrites := rewritebudget.CurrentActiveCount()
	snapshot := htmlRewriteAutoTuner.snapshot()
	plan := deriveHTMLRewritePlanWithSnapshot(
		memory,
		-1,
		maxInt64Value(totalActiveRewrites, 1),
		snapshot,
	)

	reserveBytes := int64(0)
	headroomBytes := int64(0)
	usableShare := defaultRewriteUsableShare
	if memory.MemoryBudgetBytes > 0 {
		reserveBytes = computeRewriteReserveBytes(memory.MemoryBudgetBytes, snapshot)
		headroomBytes = memory.MemoryBudgetBytes - memory.EffectiveUsedBytes - reserveBytes
		if headroomBytes < 0 {
			headroomBytes = 0
		}
		usableShare = computeRewriteUsableShare(memory, headroomBytes, maxInt64Value(totalActiveRewrites, 1), snapshot)
	}

	return HTMLRewriteStatus{
		BudgetSource:         memory.BudgetSource,
		MemoryBudgetBytes:    memory.MemoryBudgetBytes,
		GoMemoryUsedBytes:    memory.GoMemoryUsedBytes,
		EffectiveUsedBytes:   memory.EffectiveUsedBytes,
		CgroupCurrentBytes:   memory.CgroupCurrentBytes,
		RewriteReserveBytes:  reserveBytes,
		HeadroomBytes:        headroomBytes,
		BufferedLimitBytes:   plan.BufferedLimit,
		StreamChunkBytes:     plan.StreamChunkBytes,
		UnknownLengthStreams: true,

		ActiveRewrites:                 totalActiveRewrites,
		BufferedCostMultiplierMilli:    int(computeBufferedRewriteCostMultiplier(snapshot) * 1000),
		UsableShareMilli:               int(usableShare * 1000),
		RecentHTMLP90Bytes:             clampFloat64ToInt64(snapshot.KnownHTMLP90Bytes),
		BufferedSamples:                snapshot.BufferedSamples,
		StreamingSamples:               snapshot.StreamingSamples,
		BufferedThroughputBytesPerSec:  nsPerByteToBytesPerSecond(snapshot.BufferedNsPerByte),
		StreamingThroughputBytesPerSec: nsPerByteToBytesPerSecond(snapshot.StreamNsPerByte),
		CgroupHighEvents:               memory.CgroupHighEvents,
		CgroupMaxEvents:                memory.CgroupMaxEvents,
		CgroupOOMEvents:                memory.CgroupOOMEvents,
		CgroupOOMKillEvents:            memory.CgroupOOMKillEvents,
	}
}

func deriveHTMLRewritePlan(memoryBudgetBytes, memoryUsedBytes, contentLength int64) htmlRewritePlan {
	return deriveHTMLRewritePlanWithConcurrency(memoryBudgetBytes, memoryUsedBytes, contentLength, 1)
}

func deriveHTMLRewritePlanWithConcurrency(memoryBudgetBytes, memoryUsedBytes, contentLength, activeRewrites int64) htmlRewritePlan {
	return deriveHTMLRewritePlanWithSnapshot(
		rewritebudget.MemoryStatus{
			MemoryBudgetBytes:  memoryBudgetBytes,
			GoMemoryUsedBytes:  memoryUsedBytes,
			EffectiveUsedBytes: memoryUsedBytes,
		},
		contentLength,
		activeRewrites,
		defaultHTMLRewriteTuningSnapshot(),
	)
}

func deriveHTMLRewritePlanWithSnapshot(memory rewritebudget.MemoryStatus, contentLength, activeRewrites int64, snapshot htmlRewriteTuningSnapshot) htmlRewritePlan {
	plan := htmlRewritePlan{
		BufferedLimit:    defaultBufferedRewriteBytes,
		StreamChunkBytes: maxStreamRewriteChunkBytes,
	}
	usableBudget := int64(-1)
	bufferedCostMultiplier := computeBufferedRewriteCostMultiplier(snapshot)

	if memory.MemoryBudgetBytes > 0 && memory.EffectiveUsedBytes >= 0 && memory.MemoryBudgetBytes > memory.EffectiveUsedBytes {
		headroom := memory.MemoryBudgetBytes - memory.EffectiveUsedBytes - computeRewriteReserveBytes(memory.MemoryBudgetBytes, snapshot)
		if headroom <= 0 {
			usableBudget = 0
			plan.BufferedLimit = 0
			plan.StreamChunkBytes = minStreamRewriteChunkBytes
		} else {
			headroom = reduceHeadroomForInflightWeight(memory, headroom, activeRewrites)
			usableShare := computeRewriteUsableShare(memory, headroom, activeRewrites, snapshot)
			usableBudget = computeRewriteUsableBudgetBytes(headroom, activeRewrites, usableShare)
			plan.BufferedLimit = clampInt64(
				maxBufferedContentLengthForBudget(usableBudget, bufferedCostMultiplier),
				minBufferedRewriteBytes,
				maxBufferedRewriteBytes,
			)
			plan.StreamChunkBytes = deriveStreamRewriteChunkBytes(usableBudget, snapshot)
		}
	}

	plan.Buffered = contentLength >= 0 && plan.BufferedLimit > 0 && contentLength <= plan.BufferedLimit
	if plan.Buffered && usableBudget >= 0 {
		decisionBudget := clampFloat64ToInt64(float64(usableBudget) * computeBufferedDecisionFraction(snapshot))
		estimatedBufferedBytes := estimatedBufferedRewriteCostBytes(contentLength, bufferedCostMultiplier, snapshot)
		plan.Buffered = estimatedBufferedBytes <= maxInt64Value(decisionBudget, 0)
		if plan.Buffered && rewriteObjectiveReady(snapshot) {
			bufferedScore, streamingScore := compareRewriteStrategies(memory, snapshot, contentLength, decisionBudget, plan.StreamChunkBytes)
			plan.Buffered = bufferedScore <= streamingScore
		}
	}
	if plan.Buffered {
		plan.EstimatedCostBytes = estimatedBufferedRewriteCostBytes(contentLength, bufferedCostMultiplier, snapshot)
	} else {
		plan.EstimatedCostBytes = estimatedStreamingRewriteCostBytes(plan.StreamChunkBytes)
	}
	return plan
}

func reduceHeadroomForInflightWeight(memory rewritebudget.MemoryStatus, headroomBytes, activeRewrites int64) int64 {
	if headroomBytes <= 0 {
		return 0
	}
	guardBytes := rewritebudget.PredictiveRewriteGuardBytes(memory, activeRewrites)
	if guardBytes <= 0 {
		return headroomBytes
	}
	if guardBytes >= headroomBytes {
		return 0
	}
	return headroomBytes - guardBytes
}

func computeRewriteReserveBytes(memoryBudgetBytes int64, snapshot htmlRewriteTuningSnapshot) int64 {
	reserve := memoryBudgetBytes / 8
	if snapshot.KnownHTMLP90Bytes > 0 {
		observedReserve := clampFloat64ToInt64(snapshot.KnownHTMLP90Bytes * maxObservedHTMLReserveFactor)
		if observedReserve > reserve {
			reserve = observedReserve
		}
	}
	if reserve < minRewriteReserveBytes {
		return minRewriteReserveBytes
	}
	if reserve > maxRewriteReserveBytes {
		return maxRewriteReserveBytes
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
	return clampFloat64ToInt64(float64(perRewriteBudget) * clampFloat64(usableShare, minRewriteUsableShare, maxRewriteUsableShare))
}

func computeRewriteUsableShare(memory rewritebudget.MemoryStatus, headroomBytes, activeRewrites int64, snapshot htmlRewriteTuningSnapshot) float64 {
	share := defaultRewriteUsableShare
	confidence := htmlRewriteCalibrationConfidence(snapshot)

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
	if memory.MemoryBudgetBytes > 0 && memory.ActiveRewriteWeightBytes > 0 {
		weightRatio := float64(memory.ActiveRewriteWeightBytes) / float64(memory.MemoryBudgetBytes)
		if weightRatio > 0.08 {
			share -= clampFloat64((weightRatio-0.08)*0.30, 0, 0.07)
		}
	}
	if memory.AdaptivePressureMilli > 0 {
		share -= clampFloat64(float64(memory.AdaptivePressureMilli)/1000*0.10, 0, 0.10)
	}

	speedGain := bufferedSpeedGainRatio(snapshot)
	share += confidence * clampFloat64(speedGain*0.18, 0, 0.06)

	if activeRewrites > 4 {
		share -= clampFloat64(float64(activeRewrites-4)*0.01, 0, 0.08)
	}

	return clampFloat64(share, minRewriteUsableShare, maxRewriteUsableShare)
}

func estimatedBufferedRewriteCostBytes(contentLength int64, costMultiplier float64, snapshot htmlRewriteTuningSnapshot) int64 {
	if predicted := predictedBufferedRewritePeakBytes(contentLength, snapshot); predicted > 0 {
		return predicted
	}
	if contentLength <= 0 {
		return bufferedRewriteFixedCostBytes
	}
	extra := clampFloat64ToInt64(float64(contentLength) * clampFloat64(costMultiplier, minBufferedRewriteCostMultiplier, maxBufferedRewriteCostMultiplier))
	if extra >= maxInt64-bufferedRewriteFixedCostBytes {
		return maxInt64
	}
	return bufferedRewriteFixedCostBytes + extra
}

func computeBufferedRewriteCostMultiplier(snapshot htmlRewriteTuningSnapshot) float64 {
	structural := 2.25 + clampFloat64(snapshot.BufferedExpansionRatio, 0.85, 1.50)
	observed := maxFloat64(
		snapshot.BufferedPeakP95*1.01,
		snapshot.BufferedPeakP99*0.99,
	)
	if snapshot.BufferedPeakModelSamples >= 4 {
		refBytes := referenceRewriteBodyBytes(snapshot)
		if refBytes > 0 {
			regressed := float64(predictedBufferedRewritePeakBytes(refBytes, snapshot)) / float64(refBytes)
			observed = maxFloat64(observed, regressed)
		}
	}
	if snapshot.BufferedSamples < 4 {
		observed = maxFloat64(observed, defaultBufferedRewriteCostMultiplier)
	}
	return clampFloat64(maxFloat64(structural, observed), minBufferedRewriteCostMultiplier, maxBufferedRewriteCostMultiplier)
}

func maxBufferedContentLengthForBudget(usableBudgetBytes int64, costMultiplier float64) int64 {
	if usableBudgetBytes <= bufferedRewriteFixedCostBytes {
		return 0
	}
	return clampFloat64ToInt64(
		float64(usableBudgetBytes-bufferedRewriteFixedCostBytes) / clampFloat64(costMultiplier, minBufferedRewriteCostMultiplier, maxBufferedRewriteCostMultiplier),
	)
}

func computeBufferedDecisionFraction(snapshot htmlRewriteTuningSnapshot) float64 {
	confidence := htmlRewriteCalibrationConfidence(snapshot)
	speedGain := bufferedSpeedGainRatio(snapshot)
	return clampFloat64(0.82+confidence*speedGain*0.45, 0.82, 0.97)
}

func predictedBufferedRewritePeakBytes(contentLength int64, snapshot htmlRewriteTuningSnapshot) int64 {
	if snapshot.BufferedPeakModelSamples < 6 {
		return 0
	}
	x := bytesToModelUnits(maxInt64Value(contentLength, 0))
	predicted := snapshot.BufferedPeakIntercept + snapshot.BufferedPeakSlope*x + snapshot.BufferedPeakResidualP95
	if predicted <= 0 {
		return 0
	}
	return clampFloat64ToInt64(predicted)
}

func predictedBufferedDurationNs(contentLength int64, snapshot htmlRewriteTuningSnapshot) float64 {
	if snapshot.BufferedDurationModelSamples < 4 {
		if contentLength <= 0 {
			return 0
		}
		return float64(contentLength) * positiveOrDefaultFloat64(snapshot.BufferedNsPerByte, defaultBufferedRewriteNsPerByte)
	}
	x := bytesToModelUnits(maxInt64Value(contentLength, 0))
	predicted := snapshot.BufferedDurationIntercept + float64(contentLength)*snapshot.BufferedDurationSlope + x*0
	if predicted <= 0 && contentLength > 0 {
		predicted = float64(contentLength) * positiveOrDefaultFloat64(snapshot.BufferedNsPerByte, defaultBufferedRewriteNsPerByte)
	}
	return maxFloat64(predicted, 0)
}

func predictedStreamingDurationNs(contentLength int64, snapshot htmlRewriteTuningSnapshot) float64 {
	if snapshot.StreamingDurationModelSamples < 4 {
		if contentLength <= 0 {
			return 0
		}
		return float64(contentLength) * positiveOrDefaultFloat64(snapshot.StreamNsPerByte, defaultStreamRewriteNsPerByte)
	}
	x := bytesToModelUnits(maxInt64Value(contentLength, 0))
	predicted := snapshot.StreamingDurationIntercept + float64(contentLength)*snapshot.StreamingDurationSlope + x*0
	if predicted <= 0 && contentLength > 0 {
		predicted = float64(contentLength) * positiveOrDefaultFloat64(snapshot.StreamNsPerByte, defaultStreamRewriteNsPerByte)
	}
	return maxFloat64(predicted, 0)
}

func rewriteObjectiveReady(snapshot htmlRewriteTuningSnapshot) bool {
	return snapshot.BufferedPeakModelSamples >= 6 &&
		snapshot.BufferedDurationModelSamples >= 4 &&
		snapshot.StreamingDurationModelSamples >= 4
}

func compareRewriteStrategies(memory rewritebudget.MemoryStatus, snapshot htmlRewriteTuningSnapshot, contentLength, usableBudgetBytes int64, streamChunkBytes int) (float64, float64) {
	bufferedBytes := estimatedBufferedRewriteCostBytes(contentLength, computeBufferedRewriteCostMultiplier(snapshot), snapshot)
	streamBytes := estimatedStreamingRewriteCostBytes(streamChunkBytes)
	bufferedNs := predictedBufferedDurationNs(contentLength, snapshot)
	streamingNs := predictedStreamingDurationNs(contentLength, snapshot)

	bufferedCPU := normalizedCPUCost(bufferedNs, contentLength)
	streamingCPU := normalizedCPUCost(streamingNs, contentLength)
	baseCPU := positiveOrDefaultFloat64(minPositiveFloat64(bufferedCPU, streamingCPU), 1)
	baseLatency := positiveOrDefaultFloat64(minPositiveFloat64(bufferedNs, streamingNs), 1)

	pressure := rewritePressureIndex(memory)
	memWeight := 1 + 4*pressure

	bufferedScore := memWeight*memoryRiskScore(bufferedBytes, usableBudgetBytes, pressure) + bufferedCPU/baseCPU + bufferedNs/baseLatency
	streamingScore := memWeight*memoryRiskScore(streamBytes, usableBudgetBytes, pressure) + streamingCPU/baseCPU + streamingNs/baseLatency
	return bufferedScore, streamingScore
}

func estimatedStreamingRewriteCostBytes(streamChunkBytes int) int64 {
	return streamRewriteFixedCostBytes + int64(clampInt(streamChunkBytes, minStreamRewriteChunkBytes, maxStreamRewriteChunkBytes))*2
}

func normalizedCPUCost(durationNs float64, contentLength int64) float64 {
	if durationNs <= 0 {
		return 0
	}
	sizeUnits := maxFloat64(float64(maxInt64Value(contentLength, 0))/float64(128<<10), 1)
	return durationNs / sizeUnits
}

func memoryRiskScore(costBytes, usableBudgetBytes int64, pressure float64) float64 {
	if costBytes <= 0 {
		return 0
	}
	if usableBudgetBytes <= 0 {
		return 1_000_000 * (1 + pressure)
	}
	ratio := float64(costBytes) / float64(usableBudgetBytes)
	if ratio >= 1 {
		return 1_000_000 * (1 + pressure) * ratio
	}
	barrier := maxFloat64(1-ratio, 0.02)
	return (1 + pressure) * (ratio * ratio / barrier)
}

func rewritePressureIndex(memory rewritebudget.MemoryStatus) float64 {
	return rewritebudget.RuntimePressureIndex(memory)
}

func referenceRewriteBodyBytes(snapshot htmlRewriteTuningSnapshot) int64 {
	if snapshot.KnownHTMLP90Bytes > 0 {
		return clampInt64(clampFloat64ToInt64(snapshot.KnownHTMLP90Bytes), minBufferedRewriteBytes, maxBufferedRewriteBytes)
	}
	return 1 << 20
}

func bytesToModelUnits(n int64) float64 {
	if n <= 0 {
		return 0
	}
	return float64(n) / modelByteUnit
}

func modelUnitsToBytes(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return v * modelByteUnit
}

func durationToModelUnits(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(d.Nanoseconds()) / 1e6
}

func durationModelUnitsToNs(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return v * 1e6
}

func durationModelSlopeUnitsToNsPerByte(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return v * 1e6 / modelByteUnit
}

func modelPredictBytes(model adaptmodel.LinearRLSSnapshot, contentLength int64) float64 {
	if model.Samples <= 0 {
		return 0
	}
	x := bytesToModelUnits(maxInt64Value(contentLength, 0))
	return modelUnitsToBytes(model.Intercept + model.Slope*x)
}

func quantileOrDefault(q *adaptmodel.P2Quantile, fallback float64) float64 {
	if q == nil {
		return fallback
	}
	return q.Estimate(fallback)
}

func positiveDeltaFloat64(after, before float64) float64 {
	if after <= before {
		return 0
	}
	return after - before
}

func minPositiveFloat64(a, b float64) float64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func deriveStreamRewriteChunkBytes(usableBudgetBytes int64, snapshot htmlRewriteTuningSnapshot) int {
	if usableBudgetBytes <= 0 {
		return minStreamRewriteChunkBytes
	}
	workspaceBudget := usableBudgetBytes / streamWorkspaceDivisor
	if workspaceBudget > maxStreamWorkspaceBytes {
		workspaceBudget = maxStreamWorkspaceBytes
	}
	if workspaceBudget <= streamRewriteFixedCostBytes {
		return minStreamRewriteChunkBytes
	}
	chunkBudget := (workspaceBudget - streamRewriteFixedCostBytes) / streamWorkspaceChunkFactor
	baseChunk := clampInt(int(chunkBudget), minStreamRewriteChunkBytes, maxStreamRewriteChunkBytes)
	cpuBias := 1.0 + htmlRewriteCalibrationConfidence(snapshot)*clampFloat64(bufferedSpeedGainRatio(snapshot)*0.30, 0, 0.10)
	return alignStreamChunkBytes(clampInt(int(float64(baseChunk)*cpuBias), minStreamRewriteChunkBytes, maxStreamRewriteChunkBytes))
}

func htmlRewriteCalibrationConfidence(snapshot htmlRewriteTuningSnapshot) float64 {
	samples := float64(snapshot.BufferedSamples + snapshot.StreamingSamples)
	if samples <= 0 {
		return 0
	}
	return clampFloat64(samples/htmlRewriteConfidenceTargetSamples, 0, 1)
}

func bufferedSpeedGainRatio(snapshot htmlRewriteTuningSnapshot) float64 {
	buffered := positiveOrDefaultFloat64(snapshot.BufferedNsPerByte, defaultBufferedRewriteNsPerByte)
	streaming := positiveOrDefaultFloat64(snapshot.StreamNsPerByte, defaultStreamRewriteNsPerByte)
	if streaming <= 0 || buffered <= 0 || streaming <= buffered {
		return 0
	}
	return clampFloat64((streaming-buffered)/streaming, 0, 0.50)
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

func currentHTMLRewriteConcurrency() int64 {
	n := rewritebudget.CurrentActiveCount()
	if n < 1 {
		return 1
	}
	return n
}

func currentHTMLRewriteCount() int64 {
	n := activeHTMLRewriteCount.Load()
	if n < 0 {
		return 0
	}
	return n
}

func beginHTMLRewrite(weightBytes int64) func() {
	finishGlobal := rewritebudget.BeginWeighted(weightBytes)
	activeHTMLRewriteCount.Add(1)
	return func() {
		activeHTMLRewriteCount.Add(-1)
		finishGlobal()
	}
}

func htmlRewriteDecisionBucket(contentLength int64) int {
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

func computeCacheControl(disableCache bool, contentType, cacheControl, cacheControlMedia, cacheControlText string) string {
	if disableCache {
		return "no-store"
	}
	if strings.TrimSpace(cacheControl) != "" {
		return cacheControl
	}

	ct := strings.ToLower(strings.TrimSpace(contentType))
	if isMediaContentType(ct) {
		return cacheControlMedia
	}
	if isTextContentType(ct) {
		return cacheControlText
	}
	return cacheControlMedia
}

func isMediaContentType(contentType string) bool {
	return strings.HasPrefix(contentType, "image/") ||
		strings.HasPrefix(contentType, "video/") ||
		strings.HasPrefix(contentType, "audio/") ||
		strings.HasPrefix(contentType, "font/")
}

func isTextContentType(contentType string) bool {
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	switch {
	case strings.Contains(contentType, "application/json"),
		strings.Contains(contentType, "application/javascript"),
		strings.Contains(contentType, "application/x-javascript"),
		strings.Contains(contentType, "application/xml"),
		strings.Contains(contentType, "application/xhtml+xml"),
		strings.Contains(contentType, "application/yaml"),
		strings.Contains(contentType, "application/x-yaml"),
		strings.Contains(contentType, "application/toml"),
		strings.Contains(contentType, "application/vnd.apple.mpegurl"),
		strings.Contains(contentType, "application/x-mpegurl"):
		return true
	default:
		return false
	}
}

func buildPreflightResponseHeaders(r *http.Request, requestOrigin string, runtime RuntimeConfig) http.Header {
	headers := make(http.Header)

	applyCorsHeaders(headers, requestOrigin, runtime.CorsOrigins, runtime.CorsAllowCredentials, runtime.CorsExposeHeaders)
	headers.Set("access-control-allow-methods", "GET,HEAD,OPTIONS")

	requestHeaders := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers"))
	if requestHeaders == "" {
		requestHeaders = "Range"
	}
	headers.Set("access-control-allow-headers", requestHeaders)
	headers.Set("access-control-max-age", "86400")

	appendVary(headers, "access-control-request-headers")
	headers.Set("cache-control", "no-store")

	return headers
}

func applyCorsHeaders(headers http.Header, requestOrigin string, corsOrigins CorsOrigins, corsAllowCredentials bool, corsExposeHeaders string) {
	originHeader := chooseCorsOrigin(requestOrigin, corsOrigins)
	if originHeader == "" {
		return
	}

	headers.Set("access-control-allow-origin", originHeader)
	if corsAllowCredentials && originHeader != "*" {
		headers.Set("access-control-allow-credentials", "true")
	}
	if strings.TrimSpace(corsExposeHeaders) != "" {
		headers.Set("access-control-expose-headers", corsExposeHeaders)
	}
	if originHeader != "*" {
		appendVary(headers, "origin")
	}
}

func chooseCorsOrigin(requestOrigin string, corsOrigins CorsOrigins) string {
	if corsOrigins.Kind == "" || corsOrigins.Kind == "any" {
		return "*"
	}
	if strings.TrimSpace(requestOrigin) == "" {
		return ""
	}
	if _, ok := corsOrigins.AllowList[requestOrigin]; ok {
		return requestOrigin
	}
	return ""
}

func appendVary(headers http.Header, value string) {
	const key = "vary"
	existing := strings.TrimSpace(headers.Get(key))
	if existing == "" {
		headers.Set(key, value)
		return
	}
	parts := strings.Split(existing, ",")
	for _, p := range parts {
		if strings.EqualFold(strings.TrimSpace(p), value) {
			return
		}
	}
	headers.Set(key, existing+", "+value)
}

func removeVaryHeaderValue(vary, toRemove string) string {
	needle := strings.ToLower(strings.TrimSpace(toRemove))
	parts := strings.Split(vary, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		if strings.ToLower(s) == needle {
			continue
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

func applyReplacements(text, upstreamDomain, hostName string, replaceDict map[string]string) string {
	if pair, ok := resolveSingleReplacement(replaceDict, upstreamDomain, hostName); ok {
		return strings.ReplaceAll(text, pair.Old, pair.New)
	}
	out := text
	for _, pair := range buildReplacementPairs(upstreamDomain, hostName, replaceDict) {
		out = strings.ReplaceAll(out, pair.Old, pair.New)
	}
	return out
}

type replacementPair struct {
	Old string
	New string
}

type streamReplacement struct {
	old     []byte
	new     []byte
	tail    []byte
	scratch []byte
	output  []byte
}

type countingReader struct {
	src io.Reader
	n   int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r == nil || r.src == nil {
		return 0, io.EOF
	}
	n, err := r.src.Read(p)
	r.n += int64(n)
	return n, err
}

type countingWriter struct {
	dst io.Writer
	n   int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w == nil || w.dst == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := w.dst.Write(p)
	w.n += int64(n)
	return n, err
}

func buildReplacementPairs(upstreamDomain, hostName string, replaceDict map[string]string) []replacementPair {
	if len(replaceDict) == 0 {
		return nil
	}
	if pair, ok := resolveSingleReplacement(replaceDict, upstreamDomain, hostName); ok {
		return []replacementPair{pair}
	}

	keys := make([]string, 0, len(replaceDict))
	for k := range replaceDict {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]replacementPair, 0, len(keys))
	for _, key := range keys {
		rawValue := replaceDict[key]
		resolvedKey := resolveReplacementToken(key, upstreamDomain, hostName)
		if strings.TrimSpace(resolvedKey) == "" {
			continue
		}
		resolvedValue := resolveReplacementToken(rawValue, upstreamDomain, hostName)
		if resolvedKey == resolvedValue {
			continue
		}
		pairs = append(pairs, replacementPair{Old: resolvedKey, New: resolvedValue})
	}
	return pairs
}

func resolveSingleReplacement(replaceDict map[string]string, upstreamDomain, hostName string) (replacementPair, bool) {
	if len(replaceDict) != 1 {
		return replacementPair{}, false
	}
	for key, rawValue := range replaceDict {
		resolvedKey := resolveReplacementToken(key, upstreamDomain, hostName)
		if strings.TrimSpace(resolvedKey) == "" {
			return replacementPair{}, false
		}
		resolvedValue := resolveReplacementToken(rawValue, upstreamDomain, hostName)
		if resolvedKey == resolvedValue {
			return replacementPair{}, false
		}
		return replacementPair{Old: resolvedKey, New: resolvedValue}, true
	}
	return replacementPair{}, false
}

func resolveReplacementToken(token, upstreamDomain, hostName string) string {
	switch token {
	case "$upstream", "$$upstream":
		return upstreamDomain
	case "$custom_domain", "$$custom_domain":
		return hostName
	default:
		return token
	}
}

func newStreamReplacement(oldValue, newValue string) *streamReplacement {
	return &streamReplacement{
		old: []byte(oldValue),
		new: []byte(newValue),
	}
}

func (sr *streamReplacement) transform(chunk []byte, final bool) []byte {
	if len(sr.old) == 0 {
		return append([]byte(nil), chunk...)
	}

	sr.scratch = append(sr.scratch[:0], sr.tail...)
	sr.scratch = append(sr.scratch, chunk...)
	data := sr.scratch

	flushLen := len(data)
	if !final {
		flushLen = len(data) - sr.pendingPrefixLen(data)
		if flushLen <= 0 {
			sr.tail = append(sr.tail[:0], data...)
			return nil
		}
	}

	toProcess := data[:flushLen]
	if final {
		sr.tail = sr.tail[:0]
	} else {
		sr.tail = append(sr.tail[:0], data[flushLen:]...)
	}

	if len(toProcess) == 0 {
		return nil
	}
	if !bytes.Contains(toProcess, sr.old) {
		return toProcess
	}
	sr.output = appendReplacedBytes(sr.output[:0], toProcess, sr.old, sr.new)
	return sr.output
}

func (sr *streamReplacement) pendingPrefixLen(data []byte) int {
	maxKeep := len(sr.old) - 1
	if maxKeep <= 0 {
		return 0
	}
	if len(data) < maxKeep {
		maxKeep = len(data)
	}
	for keep := maxKeep; keep > 0; keep-- {
		if bytes.HasSuffix(data, sr.old[:keep]) {
			return keep
		}
	}
	return 0
}

func appendReplacedBytes(dst, src, oldValue, newValue []byte) []byte {
	if len(oldValue) == 0 {
		return append(dst, src...)
	}
	start := 0
	for {
		idx := bytes.Index(src[start:], oldValue)
		if idx < 0 {
			return append(dst, src[start:]...)
		}
		idx += start
		dst = append(dst, src[start:idx]...)
		dst = append(dst, newValue...)
		start = idx + len(oldValue)
	}
}

func streamApplyReplacements(dst io.Writer, src io.Reader, upstreamDomain, hostName string, replaceDict map[string]string) error {
	return streamApplyReplacementsWithChunkSize(dst, src, upstreamDomain, hostName, replaceDict, maxStreamRewriteChunkBytes)
}

func streamApplyReplacementsWithChunkSize(dst io.Writer, src io.Reader, upstreamDomain, hostName string, replaceDict map[string]string, chunkSize int) error {
	if pair, ok := resolveSingleReplacement(replaceDict, upstreamDomain, hostName); ok {
		return streamApplySingleReplacementWithChunkSize(dst, src, pair, chunkSize)
	}

	pairs := buildReplacementPairs(upstreamDomain, hostName, replaceDict)
	if len(pairs) == 0 {
		_, err := io.Copy(dst, src)
		return err
	}

	streamers := make([]*streamReplacement, 0, len(pairs))
	for _, pair := range pairs {
		if pair.Old == "" {
			continue
		}
		streamers = append(streamers, newStreamReplacement(pair.Old, pair.New))
	}
	if len(streamers) == 0 {
		_, err := io.Copy(dst, src)
		return err
	}

	buf := acquireGitStreamChunkBuffer()
	defer releaseGitStreamChunkBuffer(buf)
	readBuf := buf[:clampInt(chunkSize, minStreamRewriteChunkBytes, maxStreamRewriteChunkBytes)]
	for {
		n, err := src.Read(readBuf)
		if n > 0 {
			out := readBuf[:n]
			for _, streamer := range streamers {
				out = streamer.transform(out, false)
			}
			if len(out) > 0 {
				if _, writeErr := dst.Write(out); writeErr != nil {
					return writeErr
				}
			}
		}
		if err == io.EOF {
			var out []byte
			for _, streamer := range streamers {
				out = streamer.transform(out, true)
			}
			if len(out) > 0 {
				if _, writeErr := dst.Write(out); writeErr != nil {
					return writeErr
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func streamApplySingleReplacementWithChunkSize(dst io.Writer, src io.Reader, pair replacementPair, chunkSize int) error {
	oldValue := []byte(pair.Old)
	if len(oldValue) == 0 {
		_, err := io.Copy(dst, src)
		return err
	}
	newValue := []byte(pair.New)
	buf := acquireGitStreamChunkBuffer()
	defer releaseGitStreamChunkBuffer(buf)
	readBuf := buf[:clampInt(chunkSize, minStreamRewriteChunkBytes, maxStreamRewriteChunkBytes)]
	tailCap := 0
	if len(oldValue) > 1 {
		tailCap = len(oldValue) - 1
	}
	tail := make([]byte, 0, tailCap)
	data := make([]byte, 0, len(readBuf)+tailCap)

	for {
		n, err := src.Read(readBuf)
		if n > 0 {
			final := err == io.EOF
			data = append(data[:0], tail...)
			data = append(data, readBuf[:n]...)

			if final {
				return writeSingleReplacement(dst, data, oldValue, newValue)
			}

			flushUpto := len(data) - tailCap
			if flushUpto <= 0 {
				tail = append(tail[:0], data...)
				continue
			}

			tailStart, writeErr := writeSingleReplacementChunk(dst, data, oldValue, newValue, flushUpto)
			if writeErr != nil {
				return writeErr
			}
			tail = append(tail[:0], data[tailStart:]...)
		}
		if err == io.EOF {
			if len(tail) > 0 {
				return writeSingleReplacement(dst, tail, oldValue, newValue)
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func acquireGitStreamChunkBuffer() []byte {
	buf, _ := gitStreamChunkBufferPool.Get().([]byte)
	if cap(buf) < maxStreamRewriteChunkBytes {
		return make([]byte, maxStreamRewriteChunkBytes)
	}
	return buf[:maxStreamRewriteChunkBytes]
}

func releaseGitStreamChunkBuffer(buf []byte) {
	if cap(buf) < maxStreamRewriteChunkBytes {
		return
	}
	gitStreamChunkBufferPool.Put(buf[:maxStreamRewriteChunkBytes])
}

func writeSingleReplacementChunk(dst io.Writer, data, oldValue, newValue []byte, flushUpto int) (int, error) {
	if flushUpto <= 0 {
		return 0, nil
	}
	if flushUpto >= len(data) {
		return len(data), writeSingleReplacement(dst, data, oldValue, newValue)
	}

	start := 0
	searchFrom := 0
	for {
		idx := bytes.Index(data[searchFrom:], oldValue)
		if idx < 0 {
			break
		}
		idx += searchFrom
		if idx >= flushUpto {
			break
		}
		if idx > start {
			if _, err := dst.Write(data[start:idx]); err != nil {
				return 0, err
			}
		}
		if len(newValue) > 0 {
			if _, err := dst.Write(newValue); err != nil {
				return 0, err
			}
		}
		start = idx + len(oldValue)
		searchFrom = start
	}
	if start < flushUpto {
		if _, err := dst.Write(data[start:flushUpto]); err != nil {
			return 0, err
		}
		return flushUpto, nil
	}
	return start, nil
}

func writeSingleReplacement(dst io.Writer, data, oldValue, newValue []byte) error {
	start := 0
	for {
		idx := bytes.Index(data[start:], oldValue)
		if idx < 0 {
			break
		}
		idx += start
		if idx > start {
			if _, err := dst.Write(data[start:idx]); err != nil {
				return err
			}
		}
		if len(newValue) > 0 {
			if _, err := dst.Write(newValue); err != nil {
				return err
			}
		}
		start = idx + len(oldValue)
	}
	if start < len(data) {
		if _, err := dst.Write(data[start:]); err != nil {
			return err
		}
	}
	return nil
}

func buildBadGateway(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte("Bad gateway"))
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	host := strings.TrimSpace(remoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return isLoopback(host)
}

func _unused(_ ...any) {
	// keep lints happy for unused helpers if we expand later
}
