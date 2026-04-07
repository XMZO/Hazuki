package sakuyaproxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"hazuki-go/internal/model"
)

type RuntimeConfig struct {
	Host string
	Port int

	Default   OplistInstance
	Instances []OplistInstance
}

type OplistInstance struct {
	ID     string
	Name   string
	Prefix string

	Disabled bool

	Address   string
	Token     string
	PublicURL string
}

func BuildRuntimeConfig(cfg model.AppConfig) (RuntimeConfig, error) {
	port := cfg.Ports.Sakuya
	if port == 0 {
		port = 3200
	}

	// Global disable: keep runtime but mark everything disabled/unconfigured.
	if cfg.Sakuya.Disabled {
		return RuntimeConfig{
			Host: "0.0.0.0",
			Port: port,
			Default: OplistInstance{
				ID:       "default",
				Disabled: true,
			},
			Instances: []OplistInstance{},
		}, nil
	}

	def := OplistInstance{
		ID:        "default",
		Name:      "default",
		Disabled:  cfg.Sakuya.Oplist.Disabled,
		Address:   strings.TrimSpace(cfg.Sakuya.Oplist.Address),
		Token:     cfg.Sakuya.Oplist.Token,
		PublicURL: strings.TrimSpace(cfg.Sakuya.Oplist.PublicURL),
	}
	if strings.TrimSpace(def.Address) != "" && !def.Disabled {
		def.Address = strings.TrimRight(def.Address, "/")
		u, err := url.Parse(def.Address)
		if err != nil {
			return RuntimeConfig{}, err
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return RuntimeConfig{}, errors.New("sakuya.oplist.address must start with http:// or https://")
		}
		if strings.TrimSpace(u.Host) == "" {
			return RuntimeConfig{}, errors.New("sakuya.oplist.address host is empty")
		}
	}
	if strings.TrimSpace(def.PublicURL) != "" && !def.Disabled {
		def.PublicURL = strings.TrimRight(def.PublicURL, "/")
		u, err := url.Parse(def.PublicURL)
		if err != nil {
			return RuntimeConfig{}, err
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return RuntimeConfig{}, errors.New("sakuya.oplist.publicUrl must start with http:// or https://")
		}
		if strings.TrimSpace(u.Host) == "" {
			return RuntimeConfig{}, errors.New("sakuya.oplist.publicUrl host is empty")
		}
	}

	instances := make([]OplistInstance, 0, len(cfg.Sakuya.Instances))
	for _, it := range cfg.Sakuya.Instances {
		id := strings.TrimSpace(it.ID)
		if id == "" {
			continue
		}
		prefix := strings.TrimSpace(it.Prefix)
		prefix = strings.Trim(prefix, "/")
		prefix = strings.Trim(prefix, "\\")

		inst := OplistInstance{
			ID:        id,
			Name:      strings.TrimSpace(it.Name),
			Prefix:    prefix,
			Disabled:  it.Disabled,
			Address:   strings.TrimSpace(it.Address),
			Token:     it.Token,
			PublicURL: strings.TrimSpace(it.PublicURL),
		}

		// Only validate/normalize when it's enabled; disabled instances can keep drafts.
		if !inst.Disabled {
			if strings.TrimSpace(inst.Address) != "" {
				inst.Address = strings.TrimRight(inst.Address, "/")
				u, err := url.Parse(inst.Address)
				if err != nil {
					return RuntimeConfig{}, err
				}
				if u.Scheme != "http" && u.Scheme != "https" {
					return RuntimeConfig{}, errors.New("sakuya.instances.address must start with http:// or https://")
				}
				if strings.TrimSpace(u.Host) == "" {
					return RuntimeConfig{}, errors.New("sakuya.instances.address host is empty")
				}
			}

			if strings.TrimSpace(inst.PublicURL) != "" {
				inst.PublicURL = strings.TrimRight(inst.PublicURL, "/")
				u, err := url.Parse(inst.PublicURL)
				if err != nil {
					return RuntimeConfig{}, err
				}
				if u.Scheme != "http" && u.Scheme != "https" {
					return RuntimeConfig{}, errors.New("sakuya.instances.publicUrl must start with http:// or https://")
				}
				if strings.TrimSpace(u.Host) == "" {
					return RuntimeConfig{}, errors.New("sakuya.instances.publicUrl host is empty")
				}
			}
		}

		instances = append(instances, inst)
	}

	return RuntimeConfig{
		Host:      "0.0.0.0",
		Port:      port,
		Default:   def,
		Instances: instances,
	}, nil
}

type Handler struct {
	getRuntime func() RuntimeConfig

	apiClient        *http.Client
	downloadClient   *http.Client
	downloadClientH1 *http.Client

	downloadHedgeDelay time.Duration

	maxRedirects int

	linkTimeout  time.Duration
	linkRetries  int
	linkCacheTTL time.Duration

	linkMu    sync.Mutex
	linkCache map[string]cachedLink
	linkGroup singleflight.Group
}

type HandlerOptions struct {
	GetRuntime func() RuntimeConfig

	// Optional. If nil, sensible defaults are used.
	APIClient      *http.Client
	DownloadClient *http.Client

	MaxRedirects int

	// Optional. When empty/zero, sensible defaults are used.
	LinkTimeout  time.Duration
	LinkRetries  int
	LinkCacheTTL time.Duration
}

func NewHandler(opts HandlerOptions) *Handler {
	getRuntime := opts.GetRuntime
	if getRuntime == nil {
		getRuntime = func() RuntimeConfig { return RuntimeConfig{} }
	}

	return &Handler{
		getRuntime:         getRuntime,
		apiClient:          defaultAPIClient(opts.APIClient),
		downloadClient:     defaultDownloadClient(opts.DownloadClient),
		downloadClientH1:   defaultDownloadClientHTTP1(),
		downloadHedgeDelay: 350 * time.Millisecond,
		maxRedirects:       defaultInt(opts.MaxRedirects, 10),

		linkTimeout:  defaultDuration(opts.LinkTimeout, 15*time.Second),
		linkRetries:  defaultInt(opts.LinkRetries, 1),
		linkCacheTTL: defaultDuration(opts.LinkCacheTTL, 10*time.Second),
		linkCache:    map[string]cachedLink{},
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	runtime := RuntimeConfig{}
	if h.getRuntime != nil {
		runtime = h.getRuntime()
	}
	h.serveHTTP(w, r, runtime, 0)
}

func (h *Handler) serveHTTP(w http.ResponseWriter, r *http.Request, runtime RuntimeConfig, depth int) {
	if r == nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		// continue
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Health (loopback only).
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/_hazuki/health" {
		if !isLoopbackRemoteAddr(r.RemoteAddr) {
			http.NotFound(w, r)
			return
		}
		h.writeHealth(w, r, runtime)
		return
	}

	if r.Method == http.MethodOptions {
		writeCORSPreflight(w, r)
		return
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		origin = "*"
	}

	decodedPath, err := url.PathUnescape(r.URL.EscapedPath())
	if err != nil {
		writeJSON(w, r, origin, map[string]any{"code": 400, "message": "path invalid"})
		return
	}

	inst, oplistPath, _ := pickInstance(runtime, decodedPath)
	if inst.Disabled {
		writeJSONWithStatus(w, r, origin, http.StatusBadGateway, map[string]any{"code": 502, "message": "oplist instance is disabled"})
		return
	}

	// If config isn't ready, behave like a disabled/broken upstream.
	if strings.TrimSpace(inst.Address) == "" || strings.TrimSpace(inst.Token) == "" {
		writeJSONWithStatus(w, r, origin, http.StatusBadGateway, map[string]any{"code": 502, "message": "oplist is not configured"})
		return
	}

	sign := r.URL.Query().Get("sign")
	msg := verifySign(decodedPath, sign, inst.Token, time.Now())
	if msg != "" && msg == "sign mismatch" && strings.TrimSpace(oplistPath) != "" && oplistPath != decodedPath {
		if msg2 := verifySign(oplistPath, sign, inst.Token, time.Now()); msg2 == "" {
			msg = ""
		}
	}
	if msg != "" {
		// Keep Worker behavior: HTTP 200 with app-level code.
		writeJSON(w, r, origin, map[string]any{"code": 401, "message": msg})
		return
	}

	link, err := h.fetchLinkCached(r.Context(), inst, oplistPath)
	if err != nil {
		writeJSONWithStatus(w, r, origin, http.StatusBadGateway, map[string]any{"code": 502, "message": err.Error()})
		return
	}
	if link.Code != 200 {
		// Keep Worker behavior: pass through JSON as-is (still HTTP 200).
		writeJSON(w, r, origin, link)
		return
	}
	if strings.TrimSpace(link.Data.URL) == "" {
		writeJSONWithStatus(w, r, origin, http.StatusBadGateway, map[string]any{"code": 502, "message": "oplist: empty url"})
		return
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, link.Data.URL, nil)
	if err != nil {
		writeJSONWithStatus(w, r, origin, http.StatusBadGateway, map[string]any{"code": 502, "message": "bad gateway"})
		return
	}
	upReq.Header = cloneRequestHeaders(r.Header)
	upReq.Header.Set("Accept-Encoding", "identity")
	applyLinkHeaders(upReq.Header, link.Data.Header)

	resp, err := h.doWithRedirects(upReq, runtime, inst, r, w, depth)
	if err != nil {
		writeJSONWithStatus(w, r, origin, http.StatusBadGateway, map[string]any{"code": 502, "message": err.Error()})
		return
	}
	if resp == nil {
		// Internal redirect handled by recursion.
		return
	}
	defer func() { _ = resp.Body.Close() }()

	headers := w.Header()
	copyResponseHeaders(headers, resp.Header)

	// Worker behavior: delete some upstream headers.
	headers.Del("set-cookie")
	headers.Del("pragma")
	headers.Del("expires")
	headers.Del("cache-control")
	headers.Del("x-guploader-uploadid")
	headers.Del("x-goog-hash")
	headers.Del("date")
	headers.Del("vary")

	headers.Set("access-control-allow-origin", origin)
	headers.Set("accept-ranges", "bytes")
	headers.Set("vary", "Origin")

	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		headers.Set("cache-control", "public, max-age=31536000, immutable")
	} else {
		headers.Set("cache-control", "no-store")
	}

	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

func pickInstance(runtime RuntimeConfig, requestPath string) (OplistInstance, string, bool) {
	seg, rest := splitFirstPathSegment(requestPath)
	if seg == "" {
		return runtime.Default, requestPath, false
	}

	matchIdx := -1
	for i := range runtime.Instances {
		prefix := strings.TrimSpace(runtime.Instances[i].Prefix)
		if prefix == "" {
			continue
		}
		if !strings.EqualFold(prefix, seg) {
			continue
		}

		if matchIdx == -1 {
			matchIdx = i
		}

		// Prefer the first enabled+configured instance when duplicates exist.
		it := runtime.Instances[i]
		if it.Disabled {
			continue
		}
		if strings.TrimSpace(it.Address) == "" || strings.TrimSpace(it.Token) == "" {
			continue
		}

		oplistPath := rest
		if oplistPath == "" {
			oplistPath = "/"
		}
		return it, oplistPath, true
	}

	if matchIdx != -1 {
		oplistPath := rest
		if oplistPath == "" {
			oplistPath = "/"
		}
		return runtime.Instances[matchIdx], oplistPath, true
	}

	return runtime.Default, requestPath, false
}

func splitFirstPathSegment(path string) (string, string) {
	if path == "" {
		return "", ""
	}
	p := strings.TrimPrefix(path, "/")
	if p == "" {
		return "", ""
	}
	if idx := strings.IndexByte(p, '/'); idx >= 0 {
		return p[:idx], p[idx:]
	}
	return p, ""
}

func (h *Handler) writeHealth(w http.ResponseWriter, r *http.Request, runtime RuntimeConfig) {
	instances := make([]map[string]any, 0, len(runtime.Instances))
	for _, it := range runtime.Instances {
		instances = append(instances, map[string]any{
			"id":             it.ID,
			"name":           it.Name,
			"prefix":         it.Prefix,
			"disabled":       it.Disabled,
			"address":        it.Address,
			"tokenSet":       strings.TrimSpace(it.Token) != "",
			"publicUrl":      it.PublicURL,
			"publicUrlIsSet": strings.TrimSpace(it.PublicURL) != "",
		})
	}

	payload := map[string]any{
		"ok":      true,
		"service": "sakuya",
		"host":    runtime.Host,
		"port":    runtime.Port,
		"oplist": map[string]any{
			"default": map[string]any{
				"disabled":       runtime.Default.Disabled,
				"address":        runtime.Default.Address,
				"tokenSet":       strings.TrimSpace(runtime.Default.Token) != "",
				"publicUrl":      runtime.Default.PublicURL,
				"publicUrlIsSet": strings.TrimSpace(runtime.Default.PublicURL) != "",
			},
			"instances": instances,
		},
		"time": time.Now().UTC().Format(time.RFC3339Nano),
	}
	buf, _ := json.MarshalIndent(payload, "", "  ")
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(buf)
}

type linkResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
	Data    struct {
		URL    string              `json:"url"`
		Header map[string][]string `json:"header,omitempty"`
	} `json:"data"`
}

type cachedLink struct {
	link      linkResponse
	expiresAt time.Time
}

func (h *Handler) fetchLinkCached(ctx context.Context, inst OplistInstance, path string) (linkResponse, error) {
	cacheKey := inst.Address + "|" + path
	now := time.Now()

	if cached, ok := h.getCachedLink(cacheKey, now); ok {
		return cached, nil
	}

	val, err, _ := h.linkGroup.Do(cacheKey, func() (any, error) {
		now := time.Now()
		if cached, ok := h.getCachedLink(cacheKey, now); ok {
			return cached, nil
		}

		link, err := h.fetchLinkWithRetries(ctx, inst, path)
		if err != nil {
			return linkResponse{}, err
		}
		if h.linkCacheTTL > 0 && link.Code == 200 && strings.TrimSpace(link.Data.URL) != "" {
			h.setCachedLink(cacheKey, link, now.Add(h.linkCacheTTL))
		}
		return link, nil
	})
	if err != nil {
		return linkResponse{}, err
	}
	link, _ := val.(linkResponse)
	return link, nil
}

func (h *Handler) fetchLinkWithRetries(ctx context.Context, inst OplistInstance, path string) (linkResponse, error) {
	attempts := 1
	if h.linkRetries > 0 {
		attempts += h.linkRetries
	}

	var last linkResponse
	for i := 0; i < attempts; i++ {
		link, err := h.fetchLinkOnce(ctx, inst, path)
		if err != nil {
			return linkResponse{}, err
		}
		last = link

		if link.Code == 200 || i == attempts-1 || !isRetryableLinkError(link) {
			return link, nil
		}

		// Small backoff (keep it short for streaming players).
		backoff := time.Duration(150*(i+1)) * time.Millisecond
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return last, ctx.Err()
		case <-t.C:
		}
	}
	return last, nil
}

func (h *Handler) fetchLinkOnce(ctx context.Context, inst OplistInstance, path string) (linkResponse, error) {
	timeout := h.linkTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload := struct {
		Path string `json:"path"`
	}{Path: path}
	body, _ := json.Marshal(payload)

	u := inst.Address + "/api/fs/link"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return linkResponse{}, err
	}
	req.Header.Set("content-type", "application/json;charset=UTF-8")
	req.Header.Set("authorization", inst.Token)

	resp, err := h.apiClient.Do(req)
	if err != nil {
		return linkResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return linkResponse{}, err
	}
	if len(raw) == 0 {
		return linkResponse{}, errors.New("oplist: empty api response")
	}

	var out linkResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return linkResponse{}, err
	}
	return out, nil
}

func (h *Handler) getCachedLink(key string, now time.Time) (linkResponse, bool) {
	if h == nil || h.linkCacheTTL <= 0 {
		return linkResponse{}, false
	}

	h.linkMu.Lock()
	defer h.linkMu.Unlock()

	it, ok := h.linkCache[key]
	if !ok {
		return linkResponse{}, false
	}
	if now.After(it.expiresAt) {
		delete(h.linkCache, key)
		return linkResponse{}, false
	}
	return it.link, true
}

func (h *Handler) setCachedLink(key string, link linkResponse, expiresAt time.Time) {
	if h == nil || h.linkCacheTTL <= 0 {
		return
	}

	h.linkMu.Lock()
	defer h.linkMu.Unlock()

	if len(h.linkCache) > 4096 {
		for k, v := range h.linkCache {
			if time.Now().After(v.expiresAt) {
				delete(h.linkCache, k)
			}
		}
	}

	h.linkCache[key] = cachedLink{link: link, expiresAt: expiresAt}
}

func isRetryableLinkError(link linkResponse) bool {
	if link.Code < 500 {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(link.Message))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "context cancelled") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "temporary")
}

func (h *Handler) doWithRedirects(upReq *http.Request, runtime RuntimeConfig, inst OplistInstance, originalReq *http.Request, w http.ResponseWriter, depth int) (*http.Response, error) {
	if upReq == nil {
		return nil, errors.New("bad gateway")
	}

	req := upReq
	for i := 0; i <= h.maxRedirects; i++ {
		resp, err := h.doDownload(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return resp, nil
		}

		loc := strings.TrimSpace(resp.Header.Get("Location"))
		if loc == "" {
			return resp, nil
		}

		_ = resp.Body.Close()

		nextURL, err := resolveRedirect(resp.Request.URL, loc)
		if err != nil {
			return nil, err
		}

		if isSelfRedirect(nextURL, inst, originalReq) {
			if depth >= 8 {
				return nil, errors.New("redirect loop")
			}
			h.serveHTTP(w, cloneIncomingRequest(originalReq, nextURL), runtime, depth+1)
			return nil, nil
		}

		nextReq, err := http.NewRequestWithContext(req.Context(), req.Method, nextURL.String(), nil)
		if err != nil {
			return nil, err
		}
		nextReq.Header = req.Header.Clone()
		req = nextReq
	}
	return nil, errors.New("too many redirects")
}

func (h *Handler) doDownload(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("bad gateway")
	}

	primary := h.downloadClient
	secondary := h.downloadClientH1
	forceH1 := shouldForceHTTP1(req.URL) && h.downloadClientH1 != nil
	if forceH1 {
		primary = h.downloadClientH1
		secondary = h.downloadClientH1
	}

	// Moderate: only hedge/retry for known-flaky origins (e.g. 115).
	hedgeDelay := time.Duration(0)
	retries := 0
	if forceH1 && h.downloadHedgeDelay > 0 {
		hedgeDelay = h.downloadHedgeDelay
		retries = 2
	}

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		var resp *http.Response
		var err error

		if hedgeDelay > 0 && primary != nil && secondary != nil {
			resp, err = h.doHedged(primary, secondary, req, hedgeDelay)
		} else {
			outReq, cloneErr := cloneOutgoingRequest(req)
			if cloneErr != nil {
				return nil, cloneErr
			}
			resp, err = primary.Do(outReq)
			if err != nil && !forceH1 && primary != secondary && secondary != nil && shouldRetryWithHTTP1(req.URL, err) {
				retryReq, retryErr := cloneOutgoingRequest(req)
				if retryErr == nil {
					resp, err = secondary.Do(retryReq)
				}
			}
		}

		if err == nil && resp != nil {
			return resp, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		lastErr = err

		if attempt == retries {
			break
		}
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		if !isRetryableDownloadError(err) {
			break
		}

		// Short backoff to avoid immediate thundering herd; keep it small for video players.
		backoff := time.Duration(150*(attempt+1)) * time.Millisecond
		t := time.NewTimer(backoff)
		select {
		case <-req.Context().Done():
			t.Stop()
			return nil, req.Context().Err()
		case <-t.C:
		}
	}

	if lastErr == nil {
		lastErr = errors.New("bad gateway")
	}
	return nil, lastErr
}

type hedgedResult struct {
	idx  int
	resp *http.Response
	err  error
}

func (h *Handler) doHedged(primary *http.Client, secondary *http.Client, req *http.Request, delay time.Duration) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("bad gateway")
	}

	ctx := req.Context()

	ctx1, cancel1 := context.WithCancel(ctx)
	ctx2, cancel2 := context.WithCancel(ctx)

	r1, err := cloneOutgoingRequestWithContext(req, ctx1)
	if err != nil {
		cancel1()
		cancel2()
		return nil, err
	}

	ch := make(chan hedgedResult, 2)
	go func() {
		resp, err := primary.Do(r1)
		ch <- hedgedResult{idx: 1, resp: resp, err: err}
	}()

	started2 := false
	startSecond := func() error {
		if started2 {
			return nil
		}
		r2, err := cloneOutgoingRequestWithContext(req, ctx2)
		if err != nil {
			return err
		}
		started2 = true
		go func() {
			resp, err := secondary.Do(r2)
			ch <- hedgedResult{idx: 2, resp: resp, err: err}
		}()
		return nil
	}

	timerCh := (<-chan time.Time)(nil)
	var timer *time.Timer
	if delay > 0 {
		timer = time.NewTimer(delay)
		timerCh = timer.C
	} else {
		_ = startSecond()
	}
	if timer != nil {
		defer timer.Stop()
	}

	var firstErr error
	for {
		select {
		case <-ctx.Done():
			cancel1()
			cancel2()
			return nil, ctx.Err()
		case <-timerCh:
			_ = startSecond()
			timerCh = nil
		case res := <-ch:
			if res.err == nil && res.resp != nil {
				// Cancel both contexts: the winner's response is already
				// fully received, so its context is no longer needed; the
				// loser's context must be cancelled to abort in-flight work.
				cancel1()
				cancel2()
				// Close the loser's response (if it arrives). This keeps connections from leaking.
				if started2 {
					go func() {
						other := <-ch
						if other.resp != nil {
							_ = other.resp.Body.Close()
						}
					}()
				}
				return res.resp, nil
			}
			if res.resp != nil {
				_ = res.resp.Body.Close()
			}
			if firstErr == nil && res.err != nil {
				firstErr = res.err
				_ = startSecond()
				continue
			}
			cancel1()
			cancel2()
			if res.err != nil && firstErr != nil {
				return nil, errors.New(firstErr.Error() + "; " + res.err.Error())
			}
			if res.err != nil {
				return nil, res.err
			}
			if firstErr != nil {
				return nil, firstErr
			}
			return nil, errors.New("bad gateway")
		}
	}
}

func resolveRedirect(base *url.URL, location string) (*url.URL, error) {
	u, err := url.Parse(location)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return u, nil
	}
	return base.ResolveReference(u), nil
}

func cloneIncomingRequest(orig *http.Request, u *url.URL) *http.Request {
	if orig == nil {
		return &http.Request{Method: http.MethodGet, URL: u}
	}
	r := orig.Clone(orig.Context())
	// Keep headers/body/remote addr, but change path/query/host.
	if u != nil {
		r.URL = &url.URL{
			Path:     u.Path,
			RawPath:  u.RawPath,
			RawQuery: u.RawQuery,
			Fragment: u.Fragment,
		}
		if strings.TrimSpace(u.Host) != "" {
			r.Host = u.Host
		}
	}
	return r
}

func isSelfRedirect(u *url.URL, inst OplistInstance, r *http.Request) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Host))
	if host == "" {
		return false
	}

	if strings.TrimSpace(inst.PublicURL) != "" {
		pu, err := url.Parse(inst.PublicURL)
		if err == nil && pu != nil && strings.EqualFold(pu.Host, u.Host) {
			return true
		}
	}

	if r != nil {
		reqOrigin := buildRequestOrigin(r)
		if reqOrigin != "" {
			ou, err := url.Parse(reqOrigin)
			if err == nil && ou != nil && strings.EqualFold(ou.Host, u.Host) {
				return true
			}
		}
	}

	return false
}

func buildRequestOrigin(r *http.Request) string {
	if r == nil {
		return ""
	}
	proto := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "localhost"
	}
	return proto + "://" + host
}

func verifySign(path string, sign string, token string, now time.Time) string {
	sign = strings.TrimSpace(sign)
	if sign == "" {
		return "expire missing"
	}
	parts := strings.Split(sign, ":")
	last := strings.TrimSpace(parts[len(parts)-1])
	if last == "" {
		return "expire missing"
	}
	expire, err := strconv.ParseInt(last, 10, 64)
	if err != nil {
		return "expire invalid"
	}
	if expire > 0 && expire < now.UTC().Unix() {
		return "expire expired"
	}

	expected := hmacSha256Sign(path, expire, token)
	if len(sign) != len(expected) || subtle.ConstantTimeCompare([]byte(sign), []byte(expected)) != 1 {
		return "sign mismatch"
	}
	return ""
}

func hmacSha256Sign(data string, expire int64, token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(data))
	_, _ = mac.Write([]byte(":"))
	_, _ = mac.Write([]byte(strconv.FormatInt(expire, 10)))
	sum := mac.Sum(nil)

	b64 := base64.URLEncoding.EncodeToString(sum)
	return b64 + ":" + strconv.FormatInt(expire, 10)
}

func applyLinkHeaders(dst http.Header, src map[string][]string) {
	if dst == nil || len(src) == 0 {
		return
	}
	for k, vals := range src {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		for _, v := range vals {
			dst.Set(key, v)
		}
	}
}

func writeCORSPreflight(w http.ResponseWriter, r *http.Request) {
	corsHeaders := map[string]string{
		"access-control-allow-origin":  "*",
		"access-control-allow-methods": "GET,HEAD,POST,OPTIONS",
		"access-control-max-age":       "86400",
	}
	if r != nil && r.Header.Get("Origin") != "" && r.Header.Get("Access-Control-Request-Method") != "" {
		for k, v := range corsHeaders {
			w.Header().Set(k, v)
		}
		w.Header().Set("access-control-allow-headers", r.Header.Get("Access-Control-Request-Headers"))
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("allow", "GET, HEAD, POST, OPTIONS")
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, r *http.Request, origin string, payload any) {
	writeJSONWithStatus(w, r, origin, http.StatusOK, payload)
}

func writeJSONWithStatus(w http.ResponseWriter, r *http.Request, origin string, status int, payload any) {
	b, _ := json.Marshal(payload)
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("access-control-allow-origin", origin)
	w.Header().Set("vary", "Origin")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	if r != nil && r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}

func cloneRequestHeaders(src http.Header) http.Header {
	out := make(http.Header)
	if src == nil {
		return out
	}
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

func copyResponseHeaders(dst http.Header, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for k, vals := range src {
		lk := strings.ToLower(k)
		if isHopByHopHeader(lk) {
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

func defaultInt(v int, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func defaultDuration(v time.Duration, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

func defaultAPIClient(custom *http.Client) *http.Client {
	if custom != nil {
		return custom
	}
	tr := defaultTransport()
	return &http.Client{Transport: tr}
}

func defaultDownloadClient(custom *http.Client) *http.Client {
	if custom != nil {
		return custom
	}
	tr := defaultTransport()
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func defaultDownloadClientHTTP1() *http.Client {
	tr := defaultDownloadTransport()
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 0,
	}
}

func defaultDownloadTransport() *http.Transport {
	tr := defaultTransport()
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	} else {
		cfg := tr.TLSClientConfig.Clone()
		cfg.NextProtos = []string{"http/1.1"}
		tr.TLSClientConfig = cfg
	}
	return tr
}

func shouldForceHTTP1(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return false
	}
	return strings.HasSuffix(host, ".115cdn.net") || host == "115cdn.net"
}

func shouldRetryWithHTTP1(u *url.URL, err error) bool {
	if err == nil {
		return false
	}
	if shouldForceHTTP1(u) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "tls handshake timeout") ||
		strings.Contains(msg, "handshake") ||
		strings.Contains(msg, "http2") ||
		strings.Contains(msg, "protocol error")
}

func cloneOutgoingRequest(req *http.Request) (*http.Request, error) {
	return cloneOutgoingRequestWithContext(req, req.Context())
}

func cloneOutgoingRequestWithContext(req *http.Request, ctx context.Context) (*http.Request, error) {
	if req == nil || req.URL == nil {
		return nil, errors.New("bad gateway")
	}
	out, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), nil)
	if err != nil {
		return nil, err
	}
	out.Header = req.Header.Clone()
	out.Host = req.Host
	return out, nil
}

func isRetryableDownloadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return true
		}
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "temporary")
}
