package torcherinoproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"hazuki-go/internal/model"
	"hazuki-go/internal/proxy/rewritebudget"
	"hazuki-go/internal/proxy/upstreamhttp"
	"hazuki-go/internal/rediscache"
)

type RuntimeConfig struct {
	Host string
	Port int

	DefaultTarget string
	HostMapping   map[string]string

	WorkerSecretKey       string
	WorkerSecretHeaders   []string
	WorkerSecretHeaderMap map[string]string

	ForwardClientIP     bool
	TrustCfConnectingIP bool

	RedisCache RedisCacheRuntimeConfig
}

type RedisCacheRuntimeConfig struct {
	Enabled bool

	Host string
	Port int

	MaxBodyBytes       int
	DefaultTTLSeconds  int
	MaxTTLSeconds      int
	IndexKeepLastCount int
}

func BuildRuntimeConfig(cfg model.AppConfig) (RuntimeConfig, error) {
	port := cfg.Ports.Torcherino
	if port == 0 {
		port = 3000
	}

	hostMapping := normalizeHostMapping(cfg.Torcherino.HostMapping)
	secretHeaders := normalizeHeaderNames(cfg.Torcherino.WorkerSecretHeaders)
	secretHeaderMap := normalizeHeaderMap(cfg.Torcherino.WorkerSecretHeaderMap)

	redisHost := strings.TrimSpace(cfg.Cdnjs.Redis.Host)
	if redisHost == "" {
		redisHost = "redis"
	}
	redisPort := cfg.Cdnjs.Redis.Port
	if redisPort == 0 {
		redisPort = 6379
	}

	cacheMaxBodyBytes := cfg.Torcherino.RedisCache.MaxBodyBytes
	if cacheMaxBodyBytes <= 0 {
		cacheMaxBodyBytes = 512 * 1024 // 512 KiB
	}
	cacheMaxTTLSeconds := cfg.Torcherino.RedisCache.MaxTTLSeconds
	if cacheMaxTTLSeconds <= 0 {
		cacheMaxTTLSeconds = 86400 // 24h
	}

	return RuntimeConfig{
		Host: "0.0.0.0",
		Port: port,

		DefaultTarget: strings.TrimSpace(cfg.Torcherino.DefaultTarget),
		HostMapping:   hostMapping,

		WorkerSecretKey:       cfg.Torcherino.WorkerSecretKey,
		WorkerSecretHeaders:   secretHeaders,
		WorkerSecretHeaderMap: secretHeaderMap,
		ForwardClientIP:       cfg.Torcherino.ForwardClientIP,
		TrustCfConnectingIP:   cfg.Torcherino.TrustCfConnectingIP,

		RedisCache: RedisCacheRuntimeConfig{
			Enabled: cfg.Torcherino.RedisCache.Enabled,
			Host:    redisHost,
			Port:    redisPort,

			MaxBodyBytes:       cacheMaxBodyBytes,
			DefaultTTLSeconds:  cfg.Torcherino.RedisCache.DefaultTTLSeconds,
			MaxTTLSeconds:      cacheMaxTTLSeconds,
			IndexKeepLastCount: 5000,
		},
	}, nil
}

func NewHandler(runtime RuntimeConfig) http.Handler {
	return NewDynamicHandler(func() RuntimeConfig { return runtime }, nil)
}

func NewDynamicHandler(getRuntime func() RuntimeConfig, getRedis func() *redis.Client) http.Handler {
	client := upstreamhttp.NewClient(upstreamhttp.Options{
		FollowRedirects: false,
		Timeout:         60 * time.Second,
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime := RuntimeConfig{}
		if getRuntime != nil {
			runtime = getRuntime()
		}
		var redisClient *redis.Client
		if getRedis != nil {
			redisClient = getRedis()
		}
		handleRequest(w, r, runtime, client, redisClient)
	})
}

var (
	pagesDevRe = regexp.MustCompile(`(?i)https?://[^/"'\s]*\.pages\.dev`)
	hfSpaceRe  = regexp.MustCompile(`(?i)https?://[^/"'\s]*\.hf\.space`)
)

func handleRequest(w http.ResponseWriter, r *http.Request, runtime RuntimeConfig, client *http.Client, redisClient *redis.Client) {
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/_hazuki/health" {
		if !isLoopbackRemoteAddr(r.RemoteAddr) {
			http.NotFound(w, r)
			return
		}

		keys := make([]string, 0, len(runtime.WorkerSecretHeaderMap))
		for k := range runtime.WorkerSecretHeaderMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		payload := map[string]any{
			"ok":                        true,
			"service":                   "torcherino",
			"host":                      runtime.Host,
			"port":                      runtime.Port,
			"defaultTargetSet":          strings.TrimSpace(runtime.DefaultTarget) != "",
			"hostMappingCount":          len(runtime.HostMapping),
			"forwardClientIp":           runtime.ForwardClientIP,
			"trustCfConnectingIp":       runtime.TrustCfConnectingIP,
			"workerSecretSet":           strings.TrimSpace(runtime.WorkerSecretKey) != "",
			"workerSecretHeaders":       runtime.WorkerSecretHeaders,
			"workerSecretHeaderMapKeys": keys,
			"redisCache": map[string]any{
				"enabled": runtime.RedisCache.Enabled,
				"host":    runtime.RedisCache.Host,
				"port":    runtime.RedisCache.Port,
			},
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

	reqHost := normalizeHostOnly(r.Host)
	targetHost := ""
	if reqHost != "" {
		targetHost = runtime.HostMapping[strings.ToLower(reqHost)]
	}
	if strings.TrimSpace(targetHost) == "" {
		targetHost = strings.TrimSpace(runtime.DefaultTarget)
	}
	if strings.TrimSpace(targetHost) == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("Bad Gateway: DEFAULT_TARGET is empty"))
		return
	}

	upstreamURL := &url.URL{
		Scheme:   "https",
		Host:     targetHost,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}

	reqOrigin := buildRequestOrigin(r)

	cacheKey := upstreamURL.String()
	cacheCfg := runtime.RedisCache
	cacheEnabled := redisClient != nil && cacheCfg.Enabled && cacheCfg.MaxBodyBytes > 0 && cacheCfg.MaxTTLSeconds > 0
	cacheEligibleReq := cacheEnabled && canRedisCacheRequest(r)
	if cacheEligibleReq {
		if body, meta, ttlSeconds, ok := loadRedisCache(r.Context(), redisClient, cacheKey); ok {
			writeCachedResponse(w, r, body, meta, ttlSeconds, reqOrigin)
			return
		}
	}

	var body io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		body = r.Body
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), body)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("Bad Gateway"))
		return
	}
	upReq.Host = targetHost
	upReq.ContentLength = r.ContentLength
	upReq.Header = cloneRequestHeaders(r.Header)
	upReq.Header.Set("Host", targetHost)
	upReq.Header.Set("Accept-Encoding", "identity")

	if strings.TrimSpace(runtime.WorkerSecretKey) != "" {
		headerNames := runtime.WorkerSecretHeaders
		if len(headerNames) == 0 {
			headerNames = []string{"x-forwarded-by-worker"}
		}
		for _, headerName := range headerNames {
			if headerName == "" {
				continue
			}
			upReq.Header.Set(headerName, runtime.WorkerSecretKey)
		}
	}
	for headerName, headerValue := range runtime.WorkerSecretHeaderMap {
		if headerName == "" || headerValue == "" {
			continue
		}
		upReq.Header.Set(headerName, headerValue)
	}

	if runtime.ForwardClientIP {
		trustedIP := getTrustedClientIP(r, runtime)
		if ip := getHazukiClientIP(r, runtime); ip != "" {
			upReq.Header.Set("X-Hazuki-Client-IP", ip)
		}
		clientIP := getClientIP(r)
		if trustedIP != "" {
			clientIP = trustedIP
		}
		if clientIP != "" {
			upReq.Header.Set("X-Real-IP", clientIP)
			if strings.TrimSpace(upReq.Header.Get("X-Forwarded-For")) == "" {
				upReq.Header.Set("X-Forwarded-For", clientIP)
			}
		}
	}

	resp, err := client.Do(upReq)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("Bad Gateway"))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		copyResponseHeaders(w.Header(), resp.Header, true)
		if loc := resp.Header.Get("Location"); strings.TrimSpace(loc) != "" {
			w.Header().Set("Location", rewriteBody(loc, reqOrigin))
		}
		w.WriteHeader(resp.StatusCode)
		return
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	rewriteKind := detectRewriteKind(contentType)
	if rewriteKind != rewriteKindNone {
		finishRewrite := beginRewrite(rewriteKind)
		defer finishRewrite()

		rewritePlan := torcherinoRewritePlanner(rewriteKind, resp.ContentLength)
		copyResponseHeaders(w.Header(), resp.Header, false)
		w.Header().Del("Content-Length")
		w.Header().Del("Transfer-Encoding")

		if rewritePlan.Buffered {
			startedAt := time.Now()
			beforeUsed := rewritebudget.CurrentMemoryStatus().GoMemoryUsedBytes
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("Bad Gateway"))
				return
			}
			afterReadUsed := rewritebudget.CurrentMemoryStatus().GoMemoryUsedBytes
			rewritten := rewriteResponseBuffered(raw, reqOrigin)
			afterRewriteUsed := rewritebudget.CurrentMemoryStatus().GoMemoryUsedBytes
			w.Header().Set("Content-Length", strconvItoa(len(rewritten)))
			w.WriteHeader(resp.StatusCode)
			if cacheEligibleReq && r.Method == http.MethodGet {
				go cacheRespIfEligible(redisClient, cacheCfg, cacheKey, resp.StatusCode, resp.Header, raw)
			}
			if r.Method == http.MethodHead {
				return
			}
			if _, err := io.WriteString(w, rewritten); err == nil {
				currentRewriteTuner(rewriteKind).observeBuffered(
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

		w.WriteHeader(resp.StatusCode)
		if r.Method == http.MethodHead {
			return
		}

		startedAt := time.Now()
		var src io.Reader = resp.Body
		var capture *limitedCaptureBuffer
		if cacheEligibleReq && r.Method == http.MethodGet && cacheCfg.MaxBodyBytes > 0 {
			capture = newLimitedCaptureBuffer(cacheCfg.MaxBodyBytes)
			src = io.TeeReader(resp.Body, capture)
		}
		countedBody := &countingReader{src: src}
		if err := streamRewriteBodyWithChunkSize(w, countedBody, reqOrigin, rewritePlan.StreamChunkBytes); err == nil {
			currentRewriteTuner(rewriteKind).observeStreaming(countedBody.n, time.Since(startedAt))
			if capture != nil {
				if raw, ok := capture.Bytes(); ok {
					go cacheRespIfEligible(redisClient, cacheCfg, cacheKey, resp.StatusCode, resp.Header, raw)
				}
			}
		}
		return
	}

	if cacheEligibleReq && r.Method == http.MethodGet {
		if tryProxyAndCacheSmallResponse(w, r, cacheKey, cacheCfg, redisClient, resp) {
			return
		}
	}

	copyResponseHeaders(w.Header(), resp.Header, true)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

func rewriteBody(body, reqOrigin string) string {
	if strings.TrimSpace(reqOrigin) == "" {
		return body
	}
	out := pagesDevRe.ReplaceAllString(body, reqOrigin)
	out = hfSpaceRe.ReplaceAllString(out, reqOrigin)
	return out
}

func buildRequestOrigin(r *http.Request) string {
	proto := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
	if proto == "" {
		proto = "http"
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "localhost"
	}
	return proto + "://" + host
}

func hasWorkerKeyHeader(r *http.Request, runtime RuntimeConfig) bool {
	if r == nil {
		return false
	}
	headerNames := runtime.WorkerSecretHeaders
	if len(headerNames) == 0 {
		headerNames = []string{"x-forwarded-by-worker"}
	}
	for _, headerName := range headerNames {
		if headerName == "" {
			continue
		}
		if r.Header.Get(headerName) == runtime.WorkerSecretKey {
			return true
		}
	}
	return false
}

func getHazukiClientIP(r *http.Request, runtime RuntimeConfig) string {
	if r == nil {
		return ""
	}
	if trusted := getTrustedClientIP(r, runtime); trusted != "" {
		return trusted
	}
	if cf := normalizeIP(strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip"))); cf != "" {
		return cf
	}
	return getClientIP(r)
}

func getTrustedClientIP(r *http.Request, runtime RuntimeConfig) string {
	if r == nil {
		return ""
	}
	if hasWorkerKeyHeader(r, runtime) {
		if ip := normalizeIP(strings.TrimSpace(r.Header.Get("X-Hazuki-Client-IP"))); ip != "" {
			return ip
		}
	}
	if runtime.TrustCfConnectingIP {
		if cf := normalizeIP(strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip"))); cf != "" {
			return cf
		}
	}
	return ""
}

func getClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	cf := normalizeIP(strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")))
	remoteIP := normalizeIP(strings.TrimSpace(r.RemoteAddr))
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if ip := normalizeIP(first); ip != "" {
			if cf != "" && remoteIP != "" && ip == remoteIP {
				return cf
			}
			return ip
		}
	}
	if cf != "" {
		return cf
	}
	if xri := normalizeIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); xri != "" {
		return xri
	}
	return remoteIP
}

func normalizeIP(value string) string {
	s := strings.TrimSpace(value)
	if s == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.Trim(s, "[]")
	ip := net.ParseIP(s)
	if ip == nil {
		return ""
	}
	return ip.String()
}

func normalizeHostOnly(hostport string) string {
	hp := strings.TrimSpace(hostport)
	if hp == "" {
		return ""
	}
	if strings.HasPrefix(hp, "[") && strings.Contains(hp, "]") {
		if h, _, err := net.SplitHostPort(hp); err == nil {
			return strings.Trim(h, "[]")
		}
		return strings.Trim(hp, "[]")
	}
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return h
	}
	if strings.Count(hp, ":") > 1 {
		return strings.Trim(hp, "[]")
	}
	return strings.Split(hp, ":")[0]
}

func isLoopbackRemoteAddr(remoteAddr string) bool {
	host := strings.TrimSpace(remoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func cloneRequestHeaders(src http.Header) http.Header {
	out := make(http.Header)
	for k, vals := range src {
		lk := strings.ToLower(k)
		if isHopByHopHeader(lk) {
			continue
		}
		for _, v := range vals {
			out.Add(k, v)
		}
	}
	return out
}

func copyResponseHeaders(dst http.Header, src http.Header, allowContentEncoding bool) {
	for k, vals := range src {
		lk := strings.ToLower(k)
		if isHopByHopHeader(lk) {
			continue
		}
		if !allowContentEncoding && lk == "content-encoding" {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func isHopByHopHeader(lowerKey string) bool {
	switch lowerKey {
	case "connection",
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

func normalizeHostMapping(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		host := strings.ToLower(strings.TrimSpace(k))
		target := strings.TrimSpace(v)
		if host == "" || target == "" {
			continue
		}
		out[host] = target
	}
	return out
}

func normalizeHeaderName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

var headerNameRe = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9a-z-]+$`)

func isValidHeaderName(name string) bool {
	return headerNameRe.MatchString(name)
}

func normalizeHeaderNames(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		name := normalizeHeaderName(raw)
		if name == "" || !isValidHeaderName(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func normalizeHeaderMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		name := normalizeHeaderName(k)
		if name == "" || !isValidHeaderName(name) {
			continue
		}
		value := strings.TrimSpace(v)
		if value == "" {
			continue
		}
		out[name] = value
	}
	return out
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [32]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + (n % 10))
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

type cachedResponseMeta struct {
	StatusCode int
	Type       string
	Headers    map[string][]string
}

func canRedisCacheRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if strings.TrimSpace(r.Header.Get("Range")) != "" {
		return false
	}
	if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		return false
	}
	if strings.TrimSpace(r.Header.Get("Cookie")) != "" {
		return false
	}
	if cc := strings.ToLower(strings.TrimSpace(r.Header.Get("Cache-Control"))); strings.Contains(cc, "no-store") {
		return false
	}
	return true
}

func loadRedisCache(ctx context.Context, client *redis.Client, cacheKey string) ([]byte, cachedResponseMeta, int, bool) {
	id := rediscache.CacheID(cacheKey)
	bodyKey := rediscache.BodyKey(rediscache.Torcherino, id)
	metaKey := rediscache.MetaKey(rediscache.Torcherino, id)

	cctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()

	pipe := client.Pipeline()
	bodyCmd := pipe.Get(cctx, bodyKey)
	metaCmd := pipe.HGetAll(cctx, metaKey)
	ttlCmd := pipe.TTL(cctx, bodyKey)
	_, _ = pipe.Exec(cctx)

	body, _ := bodyCmd.Bytes()
	if body == nil {
		return nil, cachedResponseMeta{}, 0, false
	}
	meta, _ := metaCmd.Result()
	if len(meta) == 0 {
		return nil, cachedResponseMeta{}, 0, false
	}

	ttlSeconds := 0
	if d, err := ttlCmd.Result(); err == nil && d > 0 {
		ttlSeconds = int(d.Seconds())
	}
	if ttlSeconds <= 0 {
		return nil, cachedResponseMeta{}, 0, false
	}

	headers := map[string][]string{}
	if raw := strings.TrimSpace(meta["headers"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &headers)
	}

	statusCode := 200
	if raw := strings.TrimSpace(meta["status"]); raw != "" {
		if v, ok := parsePositiveInt(raw); ok && v >= 100 && v <= 599 {
			statusCode = v
		}
	}

	typ := strings.TrimSpace(meta["type"])

	return body, cachedResponseMeta{
		StatusCode: statusCode,
		Type:       typ,
		Headers:    headers,
	}, ttlSeconds, true
}

func writeCachedResponse(w http.ResponseWriter, r *http.Request, body []byte, meta cachedResponseMeta, ttlSeconds int, reqOrigin string) {
	applyCachedHeaders(w.Header(), meta.Headers)
	w.Header().Set("X-Proxy-Cache", "HIT")
	if strings.TrimSpace(meta.Type) != "" {
		w.Header().Set("Content-Type", meta.Type)
	}
	if ttlSeconds > 0 {
		w.Header().Set("Cache-Control", "public, max-age="+strconvItoa(ttlSeconds))
	}

	statusCode := meta.StatusCode
	if statusCode <= 0 {
		statusCode = http.StatusOK
	}

	contentType := strings.ToLower(strings.TrimSpace(meta.Type))
	rewriteKind := detectRewriteKind(contentType)
	if rewriteKind != rewriteKindNone {
		finishRewrite := beginRewrite(rewriteKind)
		defer finishRewrite()

		rewritePlan := torcherinoRewritePlanner(rewriteKind, int64(len(body)))
		w.Header().Del("Content-Length")
		w.Header().Del("Transfer-Encoding")

		if rewritePlan.Buffered {
			startedAt := time.Now()
			rewritten := rewriteResponseBuffered(body, reqOrigin)
			w.Header().Set("Content-Length", strconvItoa(len(rewritten)))
			w.WriteHeader(statusCode)
			if r.Method == http.MethodHead {
				return
			}
			if _, err := io.WriteString(w, rewritten); err == nil {
				currentRewriteTuner(rewriteKind).observeBuffered(
					int64(len(body)),
					int64(len(rewritten)),
					time.Since(startedAt),
					0,
				)
			}
			return
		}

		w.WriteHeader(statusCode)
		if r.Method == http.MethodHead {
			return
		}
		startedAt := time.Now()
		if err := streamRewriteBodyWithChunkSize(w, bytes.NewReader(body), reqOrigin, rewritePlan.StreamChunkBytes); err == nil {
			currentRewriteTuner(rewriteKind).observeStreaming(int64(len(body)), time.Since(startedAt))
		}
		return
	}

	w.Header().Del("Content-Length")
	w.Header().Del("Transfer-Encoding")
	w.Header().Set("Content-Length", strconvItoa(len(body)))
	w.WriteHeader(statusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func applyCachedHeaders(dst http.Header, cached map[string][]string) {
	for k, vals := range cached {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" || isHopByHopHeader(lk) {
			continue
		}
		switch lk {
		case "content-length", "transfer-encoding":
			continue
		}
		dst.Del(k)
		for _, v := range vals {
			if strings.TrimSpace(v) == "" {
				continue
			}
			dst.Add(k, v)
		}
	}
}

func tryProxyAndCacheSmallResponse(w http.ResponseWriter, r *http.Request, cacheKey string, cacheCfg RedisCacheRuntimeConfig, redisClient *redis.Client, resp *http.Response) bool {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false
	}
	if strings.TrimSpace(resp.Header.Get("Set-Cookie")) != "" {
		return false
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if !isRedisCacheSupportedContentType(contentType) {
		return false
	}

	ttlSeconds := cacheTTLSeconds(resp.Header.Get("Cache-Control"), cacheCfg.DefaultTTLSeconds)
	if ttlSeconds <= 0 {
		return false
	}
	if ttlSeconds > cacheCfg.MaxTTLSeconds {
		ttlSeconds = cacheCfg.MaxTTLSeconds
	}
	if ttlSeconds <= 0 {
		return false
	}

	maxBytes := cacheCfg.MaxBodyBytes
	if maxBytes <= 0 {
		return false
	}
	if resp.ContentLength >= 0 && resp.ContentLength > int64(maxBytes) {
		return false
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return false
	}
	if len(raw) > maxBytes {
		return false
	}

	copyResponseHeaders(w.Header(), resp.Header, true)
	w.Header().Del("Content-Length")
	w.Header().Del("Transfer-Encoding")
	w.Header().Set("Content-Length", strconvItoa(len(raw)))

	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}

	go cacheRespIfEligible(redisClient, cacheCfg, cacheKey, resp.StatusCode, resp.Header, raw)
	return true
}

func cacheRespIfEligible(client *redis.Client, cacheCfg RedisCacheRuntimeConfig, cacheKey string, statusCode int, headers http.Header, body []byte) {
	if client == nil || !cacheCfg.Enabled {
		return
	}
	if statusCode < 200 || statusCode > 299 {
		return
	}
	if cacheCfg.MaxBodyBytes > 0 && len(body) > cacheCfg.MaxBodyBytes {
		return
	}
	if headers == nil {
		return
	}
	if strings.TrimSpace(headers.Get("Set-Cookie")) != "" {
		return
	}

	contentType := strings.ToLower(strings.TrimSpace(headers.Get("Content-Type")))
	if !isRedisCacheSupportedContentType(contentType) {
		return
	}

	ttlSeconds := cacheTTLSeconds(headers.Get("Cache-Control"), cacheCfg.DefaultTTLSeconds)
	if ttlSeconds <= 0 {
		return
	}
	if ttlSeconds > cacheCfg.MaxTTLSeconds {
		ttlSeconds = cacheCfg.MaxTTLSeconds
	}
	if ttlSeconds <= 0 {
		return
	}

	filteredHeaders := make(map[string][]string, len(headers))
	for k, vals := range headers {
		lk := strings.ToLower(k)
		if isHopByHopHeader(lk) {
			continue
		}
		if lk == "content-length" || lk == "transfer-encoding" {
			continue
		}
		if len(vals) == 0 {
			continue
		}
		copied := make([]string, 0, len(vals))
		for _, v := range vals {
			if strings.TrimSpace(v) == "" {
				continue
			}
			copied = append(copied, v)
		}
		if len(copied) > 0 {
			filteredHeaders[k] = copied
		}
	}
	headersJSON, _ := json.Marshal(filteredHeaders)

	nowUnix := time.Now().UTC().Unix()
	id := rediscache.CacheID(cacheKey)
	bodyKey := rediscache.BodyKey(rediscache.Torcherino, id)
	metaKey := rediscache.MetaKey(rediscache.Torcherino, id)

	cctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()

	ttl := time.Duration(ttlSeconds) * time.Second
	pipe := client.Pipeline()
	pipe.SetNX(cctx, rediscache.MarkerKey, rediscache.MarkerValue, 0)
	pipe.SetEx(cctx, bodyKey, body, ttl)
	pipe.HSet(cctx, metaKey,
		"url", cacheKey,
		"type", strings.TrimSpace(headers.Get("Content-Type")),
		"size", len(body),
		"updatedAt", nowUnix,
		"status", statusCode,
		"headers", string(headersJSON),
	)
	pipe.Expire(cctx, metaKey, ttl)
	pipe.ZAdd(cctx, rediscache.Torcherino.IndexKey, redis.Z{Score: float64(nowUnix), Member: id})
	if cacheCfg.IndexKeepLastCount > 0 {
		pipe.ZRemRangeByRank(cctx, rediscache.Torcherino.IndexKey, 0, int64(-cacheCfg.IndexKeepLastCount-1))
	}
	_, _ = pipe.Exec(cctx)
}

func isRedisCacheSupportedContentType(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch {
	case strings.Contains(ct, "application/javascript"),
		strings.Contains(ct, "application/x-javascript"),
		strings.Contains(ct, "application/json"),
		strings.Contains(ct, "application/xml"),
		strings.Contains(ct, "application/xhtml+xml"),
		strings.Contains(ct, "image/"),
		strings.Contains(ct, "font/"),
		strings.Contains(ct, "application/font-"),
		strings.Contains(ct, "application/vnd.ms-fontobject"):
		return true
	default:
		return false
	}
}

func cacheTTLSeconds(cacheControl string, defaultTTLSeconds int) int {
	cc := strings.ToLower(cacheControl)
	parts := strings.Split(cc, ",")
	for _, p := range parts {
		pp := strings.TrimSpace(p)
		if pp == "no-store" || pp == "private" {
			return 0
		}
		if strings.HasPrefix(pp, "s-maxage=") {
			if n, ok := parseCacheControlInt(pp[len("s-maxage="):]); ok {
				return n
			}
		}
	}
	for _, p := range parts {
		pp := strings.TrimSpace(p)
		if strings.HasPrefix(pp, "max-age=") {
			if n, ok := parseCacheControlInt(pp[len("max-age="):]); ok {
				return n
			}
		}
	}
	if defaultTTLSeconds > 0 {
		return defaultTTLSeconds
	}
	return 0
}

func parseCacheControlInt(raw string) (int, bool) {
	s := strings.TrimSpace(strings.Trim(raw, "\""))
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
		if n > 315360000 { // 10 years
			return 315360000, true
		}
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}

func parsePositiveInt(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
		if n > 2147483647 {
			return 2147483647, true
		}
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}
