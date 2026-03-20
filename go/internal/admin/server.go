package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"hazuki-go/internal/metrics"
	"hazuki-go/internal/storage"
	"hazuki-go/internal/traffic"
)

type Options struct {
	DB         *sql.DB
	Config     *storage.ConfigStore
	Port       int
	SessionTTL int
	Metrics    *metrics.Registry
	Traffic    *traffic.Persister
}

type server struct {
	db             *sql.DB
	config         *storage.ConfigStore
	port           int
	sessionTTL     int
	startedAt      time.Time
	metrics        *metrics.Registry
	trafficPersist *traffic.Persister
}

func NewHandler(opts Options) (http.Handler, error) {
	if opts.DB == nil {
		return nil, errors.New("admin: DB is required")
	}
	if opts.Config == nil {
		return nil, errors.New("admin: ConfigStore is required")
	}
	if opts.Port < 1 || opts.Port > 65535 {
		return nil, errors.New("admin: invalid port")
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 86400
	}

	s := &server{
		db:             opts.DB,
		config:         opts.Config,
		port:           opts.Port,
		sessionTTL:     opts.SessionTTL,
		startedAt:      time.Now(),
		metrics:        opts.Metrics,
		trafficPersist: opts.Traffic,
	}

	// Best-effort cleanup.
	_ = storage.CleanupExpiredSessions(opts.DB)

	mux := http.NewServeMux()
	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(uiAssetsFS))))
	mux.Handle("/favicon.ico", http.RedirectHandler("/assets/fav.png", http.StatusFound))
	mux.Handle("/fav.png", http.RedirectHandler("/assets/fav.png", http.StatusFound))
	mux.HandleFunc("/_hazuki/health", s.wrapRequireAuth(s.health))
	mux.HandleFunc("/_hazuki/health/", s.wrapRequireAuth(s.healthSub))
	mux.HandleFunc("/_hazuki/stats", s.wrapRequireAuth(s.stats))
	mux.HandleFunc("/_hazuki/system/git-rewrite", s.wrapRequireAuth(s.systemGitRewriteStatus))
	mux.HandleFunc("/_hazuki/system/rewrite-runtime", s.wrapRequireAuth(s.systemRewriteRuntimeStatus))
	mux.HandleFunc("/setup", s.wrap(s.setup))
	mux.HandleFunc("/login", s.wrap(s.login))
	mux.HandleFunc("/lang", s.wrap(s.setLang))
	mux.HandleFunc("/logout", s.wrapRequireAuth(s.logout))
	mux.HandleFunc("/account", s.wrapRequireAuth(s.account))
	mux.HandleFunc("/account/password", s.wrapRequireAuth(s.accountPassword))
	mux.HandleFunc("/system", s.wrapRequireAuth(s.system))
	mux.HandleFunc("/system/timezone", s.wrapRequireAuth(s.systemTimeZone))
	mux.HandleFunc("/system/master-key", s.wrapRequireAuth(s.systemMasterKey))
	mux.HandleFunc("/system/master-key/verify", s.wrapRequireAuth(s.systemMasterKeyVerify))
	mux.HandleFunc("/system/redis-cache", s.wrapRequireAuth(s.redisCache))
	mux.HandleFunc("/system/redis-cache/clear", s.wrapRequireAuth(s.redisCacheClear))
	mux.HandleFunc("/system/redis-cache/delete", s.wrapRequireAuth(s.redisCacheDelete))
	mux.HandleFunc("/traffic", s.wrapRequireAuth(s.traffic))
	mux.HandleFunc("/traffic/retention", s.wrapRequireAuth(s.trafficRetention))
	mux.HandleFunc("/traffic/cleanup", s.wrapRequireAuth(s.trafficCleanup))
	mux.HandleFunc("/traffic/clear", s.wrapRequireAuth(s.trafficClear))
	mux.HandleFunc("/wizard", s.wrapRequireAuth(s.wizard))
	mux.HandleFunc("/config/git", s.wrapRequireAuth(s.configGit))
	mux.HandleFunc("/config/cdnjs", s.wrapRequireAuth(s.configCdnjs))
	mux.HandleFunc("/config/torcherino", s.wrapRequireAuth(s.configTorcherino))
	mux.HandleFunc("/config/sakuya/oplist", s.wrapRequireAuth(s.configSakuyaOplist))
	mux.HandleFunc("/config/patchouli", s.wrapRequireAuth(s.configPatchouli))
	// Backward-compatible alias: OneDrive page removed, keep old URL redirecting.
	mux.HandleFunc("/config/sakuya/onedrive", s.wrapRequireAuth(s.configSakuyaOneDrive))
	mux.HandleFunc("/config/versions", s.wrapRequireAuth(s.configVersions))
	mux.HandleFunc("/config/versions/", s.wrapRequireAuth(s.configVersionsSub))
	mux.HandleFunc("/config/export", s.wrapRequireAuth(s.configExport))
	mux.HandleFunc("/config/import", s.wrapRequireAuth(s.configImport))
	mux.HandleFunc("/_hazuki/traffic/series", s.wrapRequireAuth(s.trafficSeries))
	mux.HandleFunc("/", s.wrapRequireAuth(s.dashboard))

	return mux, nil
}
