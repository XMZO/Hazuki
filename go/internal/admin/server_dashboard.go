package admin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"hazuki-go/internal/model"
	"hazuki-go/internal/storage"
)

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	count, _ := storage.CountUsers(s.db)
	payload := map[string]any{
		"ok":                true,
		"service":           "admin",
		"port":              s.port,
		"usersCount":        count,
		"encryptionEnabled": s.config.IsEncryptionEnabled(),
		"updatedAt":         s.config.GetUpdatedAt(),
		"time":              time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}

func (s *server) healthSub(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// continue
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sub := strings.TrimPrefix(r.URL.Path, "/_hazuki/health/")
	sub = strings.Trim(sub, "/")
	if sub == "" {
		http.Redirect(w, r, "/_hazuki/health", http.StatusFound)
		return
	}

	parts := strings.Split(sub, "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	cfg, err := s.config.GetDecryptedConfig()
	if err != nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}

	service := strings.ToLower(strings.TrimSpace(parts[0]))
	port := 0
	switch service {
	case "admin":
		s.health(w, r)
		return
	case "torcherino":
		port = cfg.Ports.Torcherino
	case "cdnjs":
		port = cfg.Ports.Cdnjs
	case "git":
		port = cfg.Ports.Git
		if len(parts) >= 2 {
			instanceID := strings.TrimSpace(parts[1])
			if instanceID != "" && !strings.EqualFold(instanceID, "default") {
				found := false
				for _, it := range cfg.GitInstances {
					if strings.EqualFold(strings.TrimSpace(it.ID), instanceID) {
						port = it.Port
						found = true
						break
					}
				}
				if !found {
					http.Error(w, "Not found", http.StatusNotFound)
					return
				}
			}
		}
	case "sakuya":
		port = cfg.Ports.Sakuya
		if port == 0 {
			port = 3200
		}
	case "patchouli":
		port = cfg.Ports.Patchouli
		if port == 0 {
			port = 3201
		}
	default:
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	if port < 1 || port > 65535 {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}

	targetURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + "/_hazuki/health"
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, r.Method, targetURL, nil)
	if err != nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}

	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Only forward a minimal safe set of headers.
	if ct := strings.TrimSpace(resp.Header.Get("content-type")); ct != "" {
		w.Header().Set("content-type", ct)
	} else {
		w.Header().Set("content-type", "application/json; charset=utf-8")
	}
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 256<<10))
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// continue
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	services := map[string]storage.TrafficCounts{}
	if s.trafficPersist != nil {
		services = s.trafficPersist.TotalsNow()
	} else if s.metrics != nil {
		snap := s.metrics.Snapshot()
		for k, v := range snap {
			services[k] = storage.TrafficCounts{BytesIn: v.BytesIn, BytesOut: v.BytesOut, Requests: v.Requests}
		}
	}

	redisSt := redisStatus{Status: "disabled"}
	if cfg, err := s.config.GetDecryptedConfig(); err == nil {
		redisSt = checkRedisStatus(r.Context(), cfg.Cdnjs.Redis.Host, cfg.Cdnjs.Redis.Port)
	} else {
		redisSt = redisStatus{Status: "error", Error: err.Error()}
	}

	payload := map[string]any{
		"ok":       true,
		"time":     time.Now().UTC().Format(time.RFC3339Nano),
		"services": services,
		"redis":    redisSt,
	}
	b, _ := json.Marshal(payload)

	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}

func (s *server) dashboard(w http.ResponseWriter, r *http.Request) {
	st := getState(r.Context())
	title := s.t(r, "page.dashboard.title")

	cfg, err := s.config.GetDecryptedConfig()
	if err != nil {
		s.render(w, r, dashboardData{
			layoutData: layoutData{
				Title:        title,
				BodyTemplate: "dashboard",
				User:         st.User,
				HasUsers:     st.HasUsers,
				Error:        err.Error(),
			},
		})
		return
	}

	redisSt := checkRedisStatus(r.Context(), cfg.Cdnjs.Redis.Host, cfg.Cdnjs.Redis.Port)

	warnings := make([]string, 0, 8)
	if !s.config.IsEncryptionEnabled() {
		hasSecret := strings.TrimSpace(cfg.Git.GithubToken) != "" ||
			strings.TrimSpace(cfg.Torcherino.WorkerSecretKey) != "" ||
			strings.TrimSpace(cfg.Sakuya.Oplist.Token) != "" ||
			strings.TrimSpace(cfg.Patchouli.Token) != "" ||
			strings.TrimSpace(cfg.Patchouli.AccessKey) != "" ||
			len(cfg.Torcherino.WorkerSecretHeaderMap) > 0
		if !hasSecret {
			for _, it := range cfg.Sakuya.Instances {
				if strings.TrimSpace(it.Token) != "" {
					hasSecret = true
					break
				}
			}
		}
		if hasSecret {
			warnings = append(warnings, s.t(r, "warning.masterKeyMissing"))
		}
	}

	if !cfg.Cdnjs.Disabled {
		ghUserPolicy := strings.ToLower(strings.TrimSpace(cfg.Cdnjs.GhUserPolicy))
		if ghUserPolicy == "" {
			ghUserPolicy = "allowlist"
		}

		if strings.TrimSpace(cfg.Cdnjs.DefaultGhUser) == "" {
			warnings = append(warnings, s.t(r, "warning.cdnjs.defaultUserMissing"))
		}
		if ghUserPolicy == "denylist" {
			if len(cfg.Cdnjs.BlockedGhUsers) == 0 {
				warnings = append(warnings, s.t(r, "warning.cdnjs.denylistOpen"))
			}
		} else {
			if len(cfg.Cdnjs.AllowedGhUsers) == 0 {
				warnings = append(warnings, s.t(r, "warning.cdnjs.allowlistEmpty"))
			}
		}
		if redisSt.Status == "error" {
			warnings = append(warnings, s.t(r, "warning.cdnjs.redisError"))
		}
	}
	if !cfg.Torcherino.Disabled && strings.TrimSpace(cfg.Torcherino.DefaultTarget) == "" && len(cfg.Torcherino.HostMapping) == 0 {
		warnings = append(warnings, s.t(r, "warning.torcherino.badConfig"))
	}

	scheme := requestScheme(r)
	adminHost := strings.TrimSpace(r.Host)
	adminURL := ""
	if adminHost != "" {
		adminURL = scheme + "://" + adminHost
	}
	torcherinoURL := baseURLForPort(r, cfg.Ports.Torcherino)
	cdnjsURL := baseURLForPort(r, cfg.Ports.Cdnjs)
	gitURL := baseURLForPort(r, cfg.Ports.Git)
	sakuyaPort := cfg.Ports.Sakuya
	if sakuyaPort == 0 {
		sakuyaPort = 3200
	}
	sakuyaURL := baseURLForPort(r, sakuyaPort)
	patchouliPort := cfg.Ports.Patchouli
	if patchouliPort == 0 {
		patchouliPort = 3201
	}
	patchouliURL := baseURLForPort(r, patchouliPort)

	gitInstances := []gitInstanceRow{}
	if len(cfg.GitInstances) > 0 {
		sorted := append([]model.GitInstanceConfig(nil), cfg.GitInstances...)
		sort.Slice(sorted, func(i, j int) bool {
			return strings.ToLower(strings.TrimSpace(sorted[i].ID)) < strings.ToLower(strings.TrimSpace(sorted[j].ID))
		})
		for _, it := range sorted {
			id := strings.TrimSpace(it.ID)
			if id == "" {
				continue
			}
			name := strings.TrimSpace(it.Name)
			if name == "" {
				name = id
			}
			baseURL := baseURLForPort(r, it.Port)
			enabled := !it.Git.Disabled
			st := func() serviceStatus {
				if !enabled {
					return disabledServiceStatus(it.Port)
				}
				return checkServiceStatus(r.Context(), it.Port)
			}()
			gitInstances = append(gitInstances, gitInstanceRow{
				ID:        id,
				Name:      name,
				Port:      it.Port,
				Enabled:   enabled,
				BaseURL:   baseURL,
				HealthURL: "/_hazuki/health/git/" + url.PathEscape(id),
				Status:    st,
			})
		}
	}

	s.render(w, r, dashboardData{
		layoutData: layoutData{
			Title:        title,
			BodyTemplate: "dashboard",
			User:         st.User,
			HasUsers:     st.HasUsers,
		},
		UpdatedAt: s.config.GetUpdatedAt(),
		Ports:     cfg.Ports,
		AdminURL:  adminURL,
		AdminStatus: checkServiceStatusWithCookie(
			r.Context(),
			s.port,
			sessionCookieValue(r),
		),

		TorcherinoURL:       torcherinoURL,
		TorcherinoHealthURL: "/_hazuki/health/torcherino",
		TorcherinoStatus: func() serviceStatus {
			if cfg.Torcherino.Disabled {
				return disabledServiceStatus(cfg.Ports.Torcherino)
			}
			return checkServiceStatus(r.Context(), cfg.Ports.Torcherino)
		}(),

		CdnjsURL:       cdnjsURL,
		CdnjsHealthURL: "/_hazuki/health/cdnjs",
		CdnjsStatus: func() serviceStatus {
			if cfg.Cdnjs.Disabled {
				return disabledServiceStatus(cfg.Ports.Cdnjs)
			}
			return checkServiceStatus(r.Context(), cfg.Ports.Cdnjs)
		}(),

		GitURL:       gitURL,
		GitHealthURL: "/_hazuki/health/git",
		GitStatus: func() serviceStatus {
			if cfg.Git.Disabled {
				return disabledServiceStatus(cfg.Ports.Git)
			}
			return checkServiceStatus(r.Context(), cfg.Ports.Git)
		}(),
		GitInstances: gitInstances,

		SakuyaURL:       sakuyaURL,
		SakuyaHealthURL: "/_hazuki/health/sakuya",
		SakuyaStatus: func() serviceStatus {
			if cfg.Sakuya.Disabled {
				return disabledServiceStatus(sakuyaPort)
			}
			if !cfg.Sakuya.Oplist.Disabled &&
				strings.TrimSpace(cfg.Sakuya.Oplist.Address) != "" &&
				strings.TrimSpace(cfg.Sakuya.Oplist.Token) != "" {
				return checkServiceStatus(r.Context(), sakuyaPort)
			}
			for _, it := range cfg.Sakuya.Instances {
				if it.Disabled {
					continue
				}
				if strings.TrimSpace(it.Prefix) == "" || strings.TrimSpace(it.Address) == "" || strings.TrimSpace(it.Token) == "" {
					continue
				}
				return checkServiceStatus(r.Context(), sakuyaPort)
			}
			return disabledServiceStatus(sakuyaPort)
		}(),

		PatchouliURL:       patchouliURL,
		PatchouliHealthURL: "/_hazuki/health/patchouli",
		PatchouliStatus: func() serviceStatus {
			if cfg.Patchouli.Disabled || strings.TrimSpace(cfg.Patchouli.Repo) == "" {
				return disabledServiceStatus(patchouliPort)
			}
			return checkServiceStatus(r.Context(), patchouliPort)
		}(),

		CdnjsRedis: redisSt,
		Warnings:   warnings,
	})
}
