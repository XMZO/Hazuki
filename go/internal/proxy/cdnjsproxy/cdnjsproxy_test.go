package cdnjsproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestHandlerStreamsLargeResponseWithoutRedis(t *testing.T) {
	largeBody := strings.Repeat("console.log('hazuki');\n", 220000)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Content-Length", strconvItoa(len(largeBody)))
		_, _ = io.WriteString(w, largeBody)
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	runtime := RuntimeConfig{
		Host:              "0.0.0.0",
		Port:              3001,
		AssetURL:          upstream.URL,
		GhUserPolicy:      "allowlist",
		AllowedUsers:      map[string]struct{}{"xmzo": {}},
		BlockedUsers:      map[string]struct{}{},
		DefaultTTLSeconds: 60,
		CacheTTLSeconds:   map[string]int{},
		RedisHost:         "redis",
		RedisPort:         6379,
	}

	handler := NewDynamicHandler(func() RuntimeConfig { return runtime }, nil)
	req := httptest.NewRequest(http.MethodGet, "http://proxy.local/gh/xmzo/repo@main/file.js", nil)
	req.Host = "proxy.local"
	req.Header.Set("Accept-Encoding", "identity")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/javascript") {
		t.Fatalf("content-type = %q", got)
	}
	if got := rr.Header().Get("Content-Length"); got != strconvItoa(len(largeBody)) {
		t.Fatalf("content-length = %q, want %d", got, len(largeBody))
	}
	if got := rr.Header().Get("X-Proxy-Cache"); got != "MISS" {
		t.Fatalf("x-proxy-cache = %q, want MISS", got)
	}
	if rr.Body.Len() != len(largeBody) {
		t.Fatalf("body len = %d, want %d", rr.Body.Len(), len(largeBody))
	}
	if rr.Body.String() != largeBody {
		t.Fatalf("body mismatch from upstream %s", u.Host)
	}
}

func TestLoadCachedBodyReadsStructuredCache(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	cacheKeyID := cacheID("https://example.com/ajax/libs/lib.js")
	want := cachedBody{
		Body: []byte("console.log('hazuki');"),
		Type: "application/javascript; charset=utf-8",
	}

	cacheBufferedResponse(client, cacheKeyID, "https://example.com/ajax/libs/lib.js", 60, want)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, ok := loadCachedBody(ctx, client, cacheBodyKey(cacheKeyID), cacheTypeKey(cacheKeyID))
	if !ok {
		t.Fatal("expected cached body to be loaded")
	}
	if got.Type != want.Type {
		t.Fatalf("type = %q, want %q", got.Type, want.Type)
	}
	if string(got.Body) != string(want.Body) {
		t.Fatalf("body = %q, want %q", string(got.Body), string(want.Body))
	}
}
