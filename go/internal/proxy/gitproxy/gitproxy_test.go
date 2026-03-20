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

func TestDeriveHTMLRewritePlanStreamsWhenHeadroomIsLow(t *testing.T) {
	plan := deriveHTMLRewritePlan(256<<20, 220<<20, 512<<10)

	if plan.Buffered {
		t.Fatal("expected low-headroom plan to stream")
	}
	if plan.BufferedLimit != minBufferedRewriteBytes {
		t.Fatalf("buffered limit = %d, want %d", plan.BufferedLimit, minBufferedRewriteBytes)
	}
	if plan.StreamChunkBytes != minStreamRewriteChunkBytes {
		t.Fatalf("stream chunk = %d, want %d", plan.StreamChunkBytes, minStreamRewriteChunkBytes)
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
