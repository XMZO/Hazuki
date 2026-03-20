package gitproxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"hazuki-go/internal/proxy/rewritebudget"
)

func withHTMLRewritePlanner(t *testing.T, planner func(contentLength int64) htmlRewritePlan) {
	t.Helper()
	prev := htmlRewritePlanner
	htmlRewritePlanner = planner
	t.Cleanup(func() {
		htmlRewritePlanner = prev
	})
}

func TestHandlerRewritesSmallHTML(t *testing.T) {
	withHTMLRewritePlanner(t, func(contentLength int64) htmlRewritePlan {
		return htmlRewritePlan{
			Buffered:         true,
			BufferedLimit:    defaultBufferedRewriteBytes,
			StreamChunkBytes: maxStreamRewriteChunkBytes,
		}
	})

	var upstreamHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `<a href="https://` + upstreamHost + `/repo">repo</a>`
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	upstreamHost = u.Host

	handler := NewHandler(RuntimeConfig{
		Upstream:          u.Host,
		UpstreamMobile:    u.Host,
		UpstreamPath:      "",
		HTTPS:             u.Scheme == "https",
		CacheControlMedia: "public, max-age=43200000",
		CacheControlText:  "public, max-age=60",
		ReplaceDict:       map[string]string{"$upstream": "$custom_domain"},
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.local/index.html", nil)
	req.Host = "proxy.local"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "proxy.local") {
		t.Fatalf("expected rewritten host in body, got %q", body)
	}
	if strings.Contains(body, upstreamHost) {
		t.Fatalf("expected upstream host to be rewritten, got %q", body)
	}
}

func TestHandlerRewritesLargeHTMLStreaming(t *testing.T) {
	withHTMLRewritePlanner(t, func(contentLength int64) htmlRewritePlan {
		return htmlRewritePlan{
			Buffered:         false,
			BufferedLimit:    minBufferedRewriteBytes,
			StreamChunkBytes: minStreamRewriteChunkBytes,
		}
	})

	var upstreamHost string
	largeBody := ""

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(largeBody)))
		_, _ = io.WriteString(w, largeBody)
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	upstreamHost = u.Host
	largeBody = strings.Repeat(`<a href="https://`+upstreamHost+`/repo">repo</a>`, (2<<20)/32)

	handler := NewHandler(RuntimeConfig{
		Upstream:          u.Host,
		UpstreamMobile:    u.Host,
		UpstreamPath:      "",
		HTTPS:             u.Scheme == "https",
		CacheControlMedia: "public, max-age=43200000",
		CacheControlText:  "public, max-age=60",
		ReplaceDict:       map[string]string{"$upstream": "$custom_domain"},
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.local/index.html", nil)
	req.Host = "proxy.local"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Length"); got != "" {
		t.Fatalf("content-length = %q, want empty after streamed rewrite", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "proxy.local") {
		t.Fatalf("expected large body to be rewritten")
	}
	if strings.Contains(body, upstreamHost) {
		t.Fatalf("expected upstream host to be rewritten in large body")
	}
}

func TestDeriveHTMLRewritePlanBuffersWhenHeadroomIsHigh(t *testing.T) {
	plan := deriveHTMLRewritePlan(256<<20, 96<<20, 2<<20)

	if !plan.Buffered {
		t.Fatal("expected 2MiB body to use buffered rewrite with ample headroom")
	}
	if plan.BufferedLimit != maxBufferedRewriteBytes {
		t.Fatalf("buffered limit = %d, want %d", plan.BufferedLimit, maxBufferedRewriteBytes)
	}
	if plan.StreamChunkBytes != maxStreamRewriteChunkBytes {
		t.Fatalf("stream chunk = %d, want %d", plan.StreamChunkBytes, maxStreamRewriteChunkBytes)
	}
}

func TestDeriveHTMLRewritePlanShrinksBufferedLimitUnderPressure(t *testing.T) {
	plan := deriveHTMLRewritePlan(256<<20, 220<<20, 2<<20)

	if plan.Buffered {
		t.Fatal("expected 2MiB body to stream when the safe buffered budget shrinks")
	}
	if plan.BufferedLimit < 512<<10 || plan.BufferedLimit > 640<<10 {
		t.Fatalf("buffered limit = %d, want range [%d, %d]", plan.BufferedLimit, 512<<10, 640<<10)
	}
	if plan.StreamChunkBytes != 72<<10 {
		t.Fatalf("stream chunk = %d, want %d", plan.StreamChunkBytes, 72<<10)
	}
}

func TestDeriveHTMLRewritePlanFallsBackToDefaultsWithoutBudget(t *testing.T) {
	plan := deriveHTMLRewritePlan(0, 0, 768<<10)

	if !plan.Buffered {
		t.Fatal("expected default plan to buffer moderate HTML when no budget is available")
	}
	if plan.BufferedLimit != defaultBufferedRewriteBytes {
		t.Fatalf("buffered limit = %d, want %d", plan.BufferedLimit, defaultBufferedRewriteBytes)
	}
	if plan.StreamChunkBytes != maxStreamRewriteChunkBytes {
		t.Fatalf("stream chunk = %d, want %d", plan.StreamChunkBytes, maxStreamRewriteChunkBytes)
	}
}

func TestDeriveHTMLRewritePlanWithConcurrencyShrinksPerRequestBudget(t *testing.T) {
	single := deriveHTMLRewritePlanWithConcurrency(256<<20, 96<<20, 2<<20, 1)
	loaded := deriveHTMLRewritePlanWithConcurrency(256<<20, 96<<20, 2<<20, 32)

	if !single.Buffered {
		t.Fatal("expected single-request plan to buffer 2MiB body with ample headroom")
	}
	if loaded.Buffered {
		t.Fatal("expected concurrent rewrite load to force 2MiB body onto streaming path")
	}
	if loaded.BufferedLimit >= single.BufferedLimit {
		t.Fatalf("expected concurrent load to shrink buffered limit, got %d >= %d", loaded.BufferedLimit, single.BufferedLimit)
	}
	if loaded.StreamChunkBytes >= single.StreamChunkBytes {
		t.Fatalf("expected concurrent load to shrink stream chunk, got %d >= %d", loaded.StreamChunkBytes, single.StreamChunkBytes)
	}
}

func TestDeriveHTMLRewritePlanWithObservedPeakMultiplierShrinksBufferedLimit(t *testing.T) {
	base := deriveHTMLRewritePlanWithSnapshot(
		rewritebudget.MemoryStatus{
			MemoryBudgetBytes:  128 << 20,
			GoMemoryUsedBytes:  84 << 20,
			EffectiveUsedBytes: 84 << 20,
		},
		2<<20,
		1,
		defaultHTMLRewriteTuningSnapshot(),
	)
	tuned := deriveHTMLRewritePlanWithSnapshot(
		rewritebudget.MemoryStatus{
			MemoryBudgetBytes:  128 << 20,
			GoMemoryUsedBytes:  84 << 20,
			EffectiveUsedBytes: 84 << 20,
		},
		2<<20,
		1,
		htmlRewriteTuningSnapshot{
			BufferedExpansionRatio: 1.0,
			BufferedPeakP95:        4.20,
			BufferedPeakP99:        4.55,
			BufferedNsPerByte:      1.0,
			StreamNsPerByte:        1.8,
			BufferedSamples:        48,
			StreamingSamples:       48,
		},
	)

	if tuned.BufferedLimit >= base.BufferedLimit {
		t.Fatalf("expected calibrated higher peak memory to shrink buffered limit, got %d >= %d", tuned.BufferedLimit, base.BufferedLimit)
	}
}

func TestComputeRewriteUsableShareAndChunkGrowWhenStreamingIsMuchSlower(t *testing.T) {
	slowStream := htmlRewriteTuningSnapshot{
		BufferedExpansionRatio: 1.0,
		BufferedPeakP95:        3.3,
		BufferedPeakP99:        3.5,
		BufferedNsPerByte:      1.0,
		StreamNsPerByte:        2.0,
		BufferedSamples:        32,
		StreamingSamples:       32,
	}
	neutral := htmlRewriteTuningSnapshot{
		BufferedExpansionRatio: 1.0,
		BufferedPeakP95:        3.3,
		BufferedPeakP99:        3.5,
		BufferedNsPerByte:      1.2,
		StreamNsPerByte:        1.3,
		BufferedSamples:        32,
		StreamingSamples:       32,
	}

	memory := rewritebudget.MemoryStatus{
		MemoryBudgetBytes:  256 << 20,
		GoMemoryUsedBytes:  128 << 20,
		EffectiveUsedBytes: 128 << 20,
	}
	slowShare := computeRewriteUsableShare(memory, 128<<20, 1, slowStream)
	neutralShare := computeRewriteUsableShare(memory, 128<<20, 1, neutral)
	if slowShare <= neutralShare {
		t.Fatalf("expected measured stream slowness to increase usable share, got %.4f <= %.4f", slowShare, neutralShare)
	}

	slowChunk := deriveStreamRewriteChunkBytes(3<<20, slowStream)
	neutralChunk := deriveStreamRewriteChunkBytes(3<<20, neutral)
	if slowChunk <= neutralChunk {
		t.Fatalf("expected measured stream slowness to increase chunk size, got %d <= %d", slowChunk, neutralChunk)
	}
}

func TestHTMLRewriteAutoTunerLearnsPeakAndSpeed(t *testing.T) {
	tuner := newHTMLRewriteAutoTuner()
	tuner.observeBuffered(2<<20, (2<<20)+(32<<10), 3*time.Millisecond, 8<<20)
	tuner.observeStreaming(2<<20, 5*time.Millisecond)

	snap := tuner.snapshot()
	if snap.BufferedSamples != 1 {
		t.Fatalf("buffered samples = %d, want 1", snap.BufferedSamples)
	}
	if snap.StreamingSamples != 1 {
		t.Fatalf("streaming samples = %d, want 1", snap.StreamingSamples)
	}
	if snap.KnownHTMLP90Bytes <= 0 {
		t.Fatalf("known html p90 = %.0f, want > 0", snap.KnownHTMLP90Bytes)
	}
	if snap.BufferedPeakP99 <= defaultBufferedRewritePeakP99 {
		t.Fatalf("buffered peak p99 = %.3f, want > %.3f", snap.BufferedPeakP99, defaultBufferedRewritePeakP99)
	}
	if snap.StreamNsPerByte <= snap.BufferedNsPerByte {
		t.Fatalf("expected observed streaming path to remain slower, got %.3f <= %.3f", snap.StreamNsPerByte, snap.BufferedNsPerByte)
	}
}

func TestComputeRewriteUsableShareShrinksUnderCgroupPressure(t *testing.T) {
	snapshot := htmlRewriteTuningSnapshot{
		BufferedExpansionRatio: 1.0,
		BufferedPeakP95:        3.3,
		BufferedPeakP99:        3.5,
		BufferedNsPerByte:      1.0,
		StreamNsPerByte:        1.4,
		BufferedSamples:        48,
		StreamingSamples:       48,
	}

	quiet := rewritebudget.MemoryStatus{
		MemoryBudgetBytes:  256 << 20,
		GoMemoryUsedBytes:  96 << 20,
		EffectiveUsedBytes: 96 << 20,
		CgroupCurrentBytes: 96 << 20,
	}
	pressured := rewritebudget.MemoryStatus{
		MemoryBudgetBytes:   256 << 20,
		GoMemoryUsedBytes:   96 << 20,
		EffectiveUsedBytes:  220 << 20,
		CgroupCurrentBytes:  220 << 20,
		CgroupHighEvents:    2,
		CgroupMaxEvents:     1,
		CgroupOOMEvents:     1,
		CgroupOOMKillEvents: 1,
	}

	quietShare := computeRewriteUsableShare(quiet, 128<<20, 1, snapshot)
	pressuredShare := computeRewriteUsableShare(pressured, 36<<20, 1, snapshot)
	if pressuredShare >= quietShare {
		t.Fatalf("expected cgroup pressure to shrink usable share, got %.4f >= %.4f", pressuredShare, quietShare)
	}
}

func TestHTMLRewriteHysteresisKeepsBufferedModeNearBoundary(t *testing.T) {
	tuner := newHTMLRewriteAutoTuner()
	memory := rewritebudget.MemoryStatus{
		MemoryBudgetBytes:  256 << 20,
		GoMemoryUsedBytes:  96 << 20,
		EffectiveUsedBytes: 96 << 20,
	}
	contentLength := int64(1000 << 10)

	first := tuner.choosePlan(memory, contentLength, 1)
	if !first.Buffered {
		t.Fatal("expected first decision to buffer near the boundary with ample headroom")
	}

	// Slightly tighter headroom would normally nudge the threshold down, but hysteresis
	// should keep the same size bucket on the buffered path instead of flapping.
	memory.EffectiveUsedBytes = 118 << 20
	second := tuner.choosePlan(memory, contentLength, 1)
	if !second.Buffered {
		t.Fatal("expected hysteresis to keep buffered mode near the threshold")
	}
}

func TestStreamApplyReplacementsHandlesChunkBoundary(t *testing.T) {
	src := &chunkedReader{
		chunks: [][]byte{
			[]byte("before https://raw.github"),
			[]byte("usercontent.com/repo after"),
		},
	}
	var dst bytes.Buffer

	err := streamApplyReplacements(&dst, src, "raw.githubusercontent.com", "proxy.local", map[string]string{
		"$upstream": "$custom_domain",
	})
	if err != nil {
		t.Fatalf("streamApplyReplacements: %v", err)
	}

	got := dst.String()
	if !strings.Contains(got, "https://proxy.local/repo") {
		t.Fatalf("expected replacement across chunk boundary, got %q", got)
	}
	if strings.Contains(got, "raw.githubusercontent.com") {
		t.Fatalf("expected upstream host to be removed, got %q", got)
	}
}

func TestStreamApplyReplacementsReturnsNilOnEOF(t *testing.T) {
	err := streamApplyReplacementsWithChunkSize(io.Discard, strings.NewReader("hello"), "raw.githubusercontent.com", "proxy.local", map[string]string{
		"$upstream": "$custom_domain",
	}, minStreamRewriteChunkBytes)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

type chunkedReader struct {
	chunks [][]byte
	index  int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}
