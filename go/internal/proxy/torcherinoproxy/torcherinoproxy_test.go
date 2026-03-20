package torcherinoproxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func withTorcherinoRewritePlanner(t *testing.T, planner func(kind bodyRewriteKind, contentLength int64) bodyRewritePlan) {
	t.Helper()
	prev := torcherinoRewritePlanner
	torcherinoRewritePlanner = planner
	t.Cleanup(func() {
		torcherinoRewritePlanner = prev
	})
}

func TestTorcherinoForwardClientIP_Disabled(t *testing.T) {
	var mu sync.Mutex
	var gotXHazukiClientIP string
	var gotXRealIP string
	var gotXForwardedFor string

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotXHazukiClientIP = r.Header.Get("X-Hazuki-Client-IP")
		gotXRealIP = r.Header.Get("X-Real-IP")
		gotXForwardedFor = r.Header.Get("X-Forwarded-For")
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	runtime := RuntimeConfig{
		DefaultTarget:   u.Host,
		ForwardClientIP: false,
	}

	req := httptest.NewRequest(http.MethodGet, "http://hazuki.example/test", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("Cf-Connecting-Ip", "9.8.7.6")

	rr := httptest.NewRecorder()
	handleRequest(rr, req, runtime, ts.Client(), nil)

	mu.Lock()
	defer mu.Unlock()
	if gotXHazukiClientIP != "" {
		t.Fatalf("expected X-Hazuki-Client-IP to be empty, got %q", gotXHazukiClientIP)
	}
	if gotXRealIP != "" {
		t.Fatalf("expected X-Real-IP to be empty, got %q", gotXRealIP)
	}
	if gotXForwardedFor != "" {
		t.Fatalf("expected X-Forwarded-For to be empty, got %q", gotXForwardedFor)
	}
}

func TestTorcherinoForwardClientIP_InjectsHeaders(t *testing.T) {
	var mu sync.Mutex
	var gotXHazukiClientIP string
	var gotXRealIP string
	var gotXForwardedFor string

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotXHazukiClientIP = r.Header.Get("X-Hazuki-Client-IP")
		gotXRealIP = r.Header.Get("X-Real-IP")
		gotXForwardedFor = r.Header.Get("X-Forwarded-For")
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	runtime := RuntimeConfig{
		DefaultTarget:   u.Host,
		ForwardClientIP: true,
	}

	req := httptest.NewRequest(http.MethodGet, "http://hazuki.example/test", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("Cf-Connecting-Ip", "9.8.7.6")

	rr := httptest.NewRecorder()
	handleRequest(rr, req, runtime, ts.Client(), nil)

	mu.Lock()
	defer mu.Unlock()
	if gotXHazukiClientIP != "9.8.7.6" {
		t.Fatalf("expected X-Hazuki-Client-IP %q, got %q", "9.8.7.6", gotXHazukiClientIP)
	}
	if gotXRealIP != "9.8.7.6" {
		t.Fatalf("expected X-Real-IP %q, got %q", "9.8.7.6", gotXRealIP)
	}
	if gotXForwardedFor != "9.8.7.6" {
		t.Fatalf("expected X-Forwarded-For %q, got %q", "9.8.7.6", gotXForwardedFor)
	}
}

func TestTorcherinoForwardClientIP_DoesNotOverrideXForwardedFor(t *testing.T) {
	var mu sync.Mutex
	var gotXHazukiClientIP string
	var gotXRealIP string
	var gotXForwardedFor string

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotXHazukiClientIP = r.Header.Get("X-Hazuki-Client-IP")
		gotXRealIP = r.Header.Get("X-Real-IP")
		gotXForwardedFor = r.Header.Get("X-Forwarded-For")
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	runtime := RuntimeConfig{
		DefaultTarget:   u.Host,
		ForwardClientIP: true,
	}

	req := httptest.NewRequest(http.MethodGet, "http://hazuki.example/test", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("Cf-Connecting-Ip", "9.8.7.6")
	req.Header.Set("X-Forwarded-For", "11.11.11.11, 22.22.22.22")

	rr := httptest.NewRecorder()
	handleRequest(rr, req, runtime, ts.Client(), nil)

	mu.Lock()
	defer mu.Unlock()
	if gotXHazukiClientIP != "9.8.7.6" {
		t.Fatalf("expected X-Hazuki-Client-IP %q, got %q", "9.8.7.6", gotXHazukiClientIP)
	}
	if gotXRealIP != "11.11.11.11" {
		t.Fatalf("expected X-Real-IP %q, got %q", "11.11.11.11", gotXRealIP)
	}
	if gotXForwardedFor != "11.11.11.11, 22.22.22.22" {
		t.Fatalf("expected X-Forwarded-For to be preserved, got %q", gotXForwardedFor)
	}
}

func TestTorcherinoForwardClientIP_TrustCfConnectingIP(t *testing.T) {
	var mu sync.Mutex
	var gotXHazukiClientIP string
	var gotXRealIP string
	var gotXForwardedFor string

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotXHazukiClientIP = r.Header.Get("X-Hazuki-Client-IP")
		gotXRealIP = r.Header.Get("X-Real-IP")
		gotXForwardedFor = r.Header.Get("X-Forwarded-For")
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	runtime := RuntimeConfig{
		DefaultTarget:       u.Host,
		ForwardClientIP:     true,
		TrustCfConnectingIP: true,
	}

	req := httptest.NewRequest(http.MethodGet, "http://hazuki.example/test", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("Cf-Connecting-Ip", "9.8.7.6")
	req.Header.Set("X-Forwarded-For", "11.11.11.11, 22.22.22.22")

	rr := httptest.NewRecorder()
	handleRequest(rr, req, runtime, ts.Client(), nil)

	mu.Lock()
	defer mu.Unlock()
	if gotXHazukiClientIP != "9.8.7.6" {
		t.Fatalf("expected X-Hazuki-Client-IP %q, got %q", "9.8.7.6", gotXHazukiClientIP)
	}
	if gotXRealIP != "9.8.7.6" {
		t.Fatalf("expected X-Real-IP %q, got %q", "9.8.7.6", gotXRealIP)
	}
	if gotXForwardedFor != "11.11.11.11, 22.22.22.22" {
		t.Fatalf("expected X-Forwarded-For to be preserved, got %q", gotXForwardedFor)
	}
}

func TestTorcherinoForwardClientIP_TrustedHazukiClientIP(t *testing.T) {
	var mu sync.Mutex
	var gotXHazukiClientIP string
	var gotXRealIP string
	var gotXForwardedFor string

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotXHazukiClientIP = r.Header.Get("X-Hazuki-Client-IP")
		gotXRealIP = r.Header.Get("X-Real-IP")
		gotXForwardedFor = r.Header.Get("X-Forwarded-For")
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	runtime := RuntimeConfig{
		DefaultTarget:       u.Host,
		ForwardClientIP:     true,
		WorkerSecretKey:     "secret",
		WorkerSecretHeaders: []string{"x-forwarded-by-worker"},
	}

	req := httptest.NewRequest(http.MethodGet, "http://hazuki.example/test", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	req.Header.Set("X-Forwarded-By-Worker", "secret")
	req.Header.Set("X-Hazuki-Client-IP", "55.66.77.88")
	req.Header.Set("X-Forwarded-For", "11.11.11.11, 22.22.22.22")

	rr := httptest.NewRecorder()
	handleRequest(rr, req, runtime, ts.Client(), nil)

	mu.Lock()
	defer mu.Unlock()
	if gotXHazukiClientIP != "55.66.77.88" {
		t.Fatalf("expected X-Hazuki-Client-IP %q, got %q", "55.66.77.88", gotXHazukiClientIP)
	}
	if gotXRealIP != "55.66.77.88" {
		t.Fatalf("expected X-Real-IP %q, got %q", "55.66.77.88", gotXRealIP)
	}
	if gotXForwardedFor != "11.11.11.11, 22.22.22.22" {
		t.Fatalf("expected X-Forwarded-For to be preserved, got %q", gotXForwardedFor)
	}
}

func TestStreamRewriteBodyWithChunkBoundary(t *testing.T) {
	src := &chunkedReader{
		chunks: [][]byte{
			[]byte(`before https://exa`),
			[]byte(`mple.pages.dev/a after https://b`),
			[]byte(`eta.hf.space/x`),
		},
	}
	var dst bytes.Buffer
	if err := streamRewriteBodyWithChunkSize(&dst, src, "https://hazuki.example", minStreamRewriteChunkBytes); err != nil {
		t.Fatalf("streamRewriteBodyWithChunkSize: %v", err)
	}
	got := dst.String()
	if strings.Contains(got, ".pages.dev") || strings.Contains(got, ".hf.space") {
		t.Fatalf("expected upstream origins to be rewritten, got %q", got)
	}
	if strings.Count(got, "https://hazuki.example") != 2 {
		t.Fatalf("expected both origins to be rewritten, got %q", got)
	}
}

func TestTorcherinoHandlerRewritesLargeHTMLStreaming(t *testing.T) {
	withTorcherinoRewritePlanner(t, func(kind bodyRewriteKind, contentLength int64) bodyRewritePlan {
		return bodyRewritePlan{
			Buffered:         false,
			BufferedLimit:    minBufferedRewriteBytes,
			StreamChunkBytes: minStreamRewriteChunkBytes,
		}
	})

	largeBody := strings.Repeat(`<a href="https://demo.pages.dev/repo">repo</a>`, (2<<20)/48)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(largeBody)))
		_, _ = io.WriteString(w, largeBody)
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://hazuki.example/index.html", nil)
	req.Host = "hazuki.example"
	req.Header.Set("X-Forwarded-Proto", "https")

	rr := httptest.NewRecorder()
	handleRequest(rr, req, RuntimeConfig{DefaultTarget: u.Host}, ts.Client(), nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Length"); got != "" {
		t.Fatalf("content-length = %q, want empty after streamed rewrite", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "https://hazuki.example") {
		t.Fatalf("expected rewritten host in body")
	}
	if strings.Contains(body, ".pages.dev") {
		idx := strings.Index(body, ".pages.dev")
		start := idx - 60
		if start < 0 {
			start = 0
		}
		end := idx + 80
		if end > len(body) {
			end = len(body)
		}
		t.Fatalf("expected pages.dev origin to be removed from body, snippet=%q", body[start:end])
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
