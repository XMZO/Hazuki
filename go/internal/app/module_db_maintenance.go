package app

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"hazuki-go/internal/model"
	"hazuki-go/internal/storage"
)

type dbMaintenanceModule struct{}

func (dbMaintenanceModule) Name() string { return "db_maintenance" }

func (dbMaintenanceModule) Start(ctx context.Context, env *runtimeEnv, _ chan<- error) (*runningModule, error) {
	if env == nil || env.db == nil || env.config == nil {
		return &runningModule{name: "db_maintenance", started: false}, nil
	}

	versionsMax := parseEnvInt("HAZUKI_CONFIG_VERSIONS_MAX", 200)
	if versionsMax < 0 {
		versionsMax = 0
	}

	vacuumEveryHours := parseEnvInt("HAZUKI_DB_VACUUM_INTERVAL_HOURS", 24)
	vacuumEvery := time.Duration(vacuumEveryHours) * time.Hour
	if vacuumEveryHours <= 0 {
		vacuumEvery = 0
	}

	minFreeMB := parseEnvInt("HAZUKI_DB_VACUUM_MIN_FREE_MB", 16)
	if minFreeMB < 0 {
		minFreeMB = 0
	}
	minFreeBytes := int64(minFreeMB) * 1024 * 1024

	minFreeRatio := parseEnvFloat("HAZUKI_DB_VACUUM_MIN_FREE_RATIO", 0.20)
	if minFreeRatio < 0 {
		minFreeRatio = 0
	}
	if minFreeRatio > 0.95 {
		minFreeRatio = 0.95
	}

	pruneNow := func() {
		if versionsMax <= 0 {
			return
		}
		ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		n, err := storage.PruneConfigVersions(ctx2, env.db, versionsMax)
		if err != nil {
			log.Printf("db_maintenance: prune config versions failed: %v", err)
			return
		}
		if n > 0 {
			log.Printf("db_maintenance: pruned %d old config_versions rows (keep %d)", n, versionsMax)
		}
	}

	maybeVacuum := func() {
		if vacuumEvery <= 0 {
			return
		}

		ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		st, err := storage.ReadSQLiteStats(ctx2, env.db)
		if err != nil {
			log.Printf("db_maintenance: read sqlite stats failed: %v", err)
			return
		}
		freeBytes := st.FreeBytes()
		totalBytes := st.TotalBytes()
		if totalBytes <= 0 {
			return
		}
		freeRatio := float64(freeBytes) / float64(totalBytes)

		if minFreeBytes > 0 && freeBytes < minFreeBytes {
			return
		}
		if minFreeRatio > 0 && freeRatio < minFreeRatio {
			return
		}

		log.Printf(
			"db_maintenance: running VACUUM (free %.1f%%, free=%s, total=%s)",
			freeRatio*100,
			formatBytesLocal(freeBytes),
			formatBytesLocal(totalBytes),
		)
		if err := storage.Vacuum(ctx2, env.db); err != nil {
			log.Printf("db_maintenance: VACUUM failed: %v", err)
			return
		}
		_ = storage.OptimizeSQLite(ctx2, env.db)
	}

	pruneCh := make(chan struct{}, 1)
	env.config.OnChanged(func(_ model.AppConfig) {
		select {
		case pruneCh <- struct{}{}:
		default:
		}
	})

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("db_maintenance: goroutine panic: %v", r)
			}
		}()

		// Stagger the initial prune so it doesn't compete with startup bursts.
		startupTimer := time.NewTimer(30 * time.Second)
		defer startupTimer.Stop()

		var vacuumTicker *time.Ticker
		var vacuumC <-chan time.Time
		if vacuumEvery > 0 {
			vacuumTicker = time.NewTicker(vacuumEvery)
			defer vacuumTicker.Stop()
			vacuumC = vacuumTicker.C
		}
		pruneTicker := time.NewTicker(6 * time.Hour)
		defer pruneTicker.Stop()

		sessionCleanupTicker := time.NewTicker(1 * time.Hour)
		defer sessionCleanupTicker.Stop()

		var debounce *time.Timer
		var debounceC <-chan time.Time

		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-startupTimer.C:
				pruneNow()
				maybeVacuum()
			case <-pruneTicker.C:
				pruneNow()
			case <-pruneCh:
				if versionsMax <= 0 {
					continue
				}
				if debounce == nil {
					debounce = time.NewTimer(2 * time.Second)
					debounceC = debounce.C
					continue
				}
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounce.Reset(2 * time.Second)
			case <-debounceC:
				if debounce != nil {
					debounce.Stop()
				}
				debounce = nil
				debounceC = nil
				pruneNow()
			case <-sessionCleanupTicker.C:
				if err := storage.CleanupExpiredSessions(env.db); err != nil {
					log.Printf("db_maintenance: session cleanup failed: %v", err)
				}
			case <-vacuumC:
				// It's OK if VACUUM can't acquire an exclusive lock (busy); we'll try next time.
				maybeVacuum()
			}
		}
	}()

	return &runningModule{
		name:    "db_maintenance",
		started: true,
		shutdown: func(context.Context) error {
			close(stopCh)
			<-doneCh
			return nil
		},
	}, nil
}

func parseEnvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func parseEnvFloat(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return n
}

func formatBytesLocal(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}

	const unit = 1024
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	suffix := "KB"
	if exp >= 0 && exp < len(suffixes) {
		suffix = suffixes[exp]
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + suffix
}
