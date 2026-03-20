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

func TestHandlerRewritesSmallHTML(t *testing.T) {
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
