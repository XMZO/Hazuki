package app

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"hazuki-go/internal/metrics"
	"hazuki-go/internal/proxy/rewritebudget"
	"hazuki-go/internal/storage"
)

func Run(ctx context.Context) error {
	// Optional: load dotenv.
	// Prefer go/.env when running from repo root.
	_ = godotenv.Load(filepath.Join("go", ".env"))
	if _, err := os.Stat("go.mod"); err == nil {
		_ = godotenv.Load(".env")
	}

	dbPath := strings.TrimSpace(os.Getenv("HAZUKI_DB_PATH"))
	if dbPath == "" {
		dbPath = filepath.Join("data", "hazuki.db")
	}
	masterKey := os.Getenv("HAZUKI_MASTER_KEY")

	db, err := storage.OpenDB(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := storage.Migrate(db); err != nil {
		return err
	}

	rewritebudget.ApplyRuntimeEnv()
	restoreRewriteAutoTuneModel(ctx, db)
	rewritebudget.StartAdaptiveGCController(ctx)

	cryptoContext, err := storage.NewCryptoContext(db, masterKey)
	if err != nil {
		return err
	}

	configStore := storage.NewConfigStore(db, cryptoContext)
	if err := configStore.InitFromEnvironment(os.Getenv, os.LookupEnv); err != nil {
		return err
	}

	_, _ = storage.EnsureBootstrapAdmin(db, os.Getenv("HAZUKI_ADMIN_USERNAME"), os.Getenv("HAZUKI_ADMIN_PASSWORD"))

	appCfg, err := configStore.GetDecryptedConfig()
	if err != nil {
		return err
	}

	sessionTTL := parsePositiveInt(os.Getenv("HAZUKI_SESSION_TTL_SECONDS"), 86400)
	env := &runtimeEnv{
		db:         db,
		config:     configStore,
		initialCfg: appCfg,
		sessionTTL: sessionTTL,
		metrics:    metrics.NewRegistry(),
	}
	startRewriteAutoTune(ctx, db)

	fatalErrCh := make(chan error, 1)
	modules := []module{
		trafficModule{},
		dbMaintenanceModule{},
		adminModule{},
		torcherinoModule{},
		sakuyaModule{},
		patchouliModule{},
		gitModule{},
		cdnjsModule{},
	}

	started := make([]*runningModule, 0, len(modules))
	for _, m := range modules {
		rm, err := m.Start(ctx, env, fatalErrCh)
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, prev := range started {
				prev.Stop(shutdownCtx)
			}
			return err
		}
		started = append(started, rm)
	}

	select {
	case <-ctx.Done():
	case err := <-fatalErrCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, rm := range started {
		rm.Stop(shutdownCtx)
	}
	return nil
}

func parsePositiveInt(value string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
