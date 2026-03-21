package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"hazuki-go/internal/model"
	"hazuki-go/internal/proxy/gitproxy"
	"hazuki-go/internal/proxy/rewritebudget"
	"hazuki-go/internal/proxy/torcherinoproxy"
	"hazuki-go/internal/rediscache"
	"hazuki-go/internal/storage"
)

func (s *server) system(w http.ResponseWriter, r *http.Request) {
	st := getState(r.Context())
	title := s.t(r, "page.system.title")

	notice := ""
	if r.Method == http.MethodGet {
		if ok := strings.TrimSpace(r.URL.Query().Get("ok")); ok != "" {
			if ok == "masterKey" {
				if strings.TrimSpace(r.URL.Query().Get("env")) == "1" {
					notice = s.t(r, "system.masterKeyRotatedNoticeEnvUpdated")
				} else {
					notice = s.t(r, "system.masterKeyRotatedNotice")
				}
			} else if ok == "masterKeyVerify" {
				notice = s.t(r, "system.masterKeyVerifiedNotice")
			} else {
				notice = s.t(r, "common.saved")
			}
		}
	}
	errMsg := ""
	if r.Method == http.MethodGet {
		switch strings.TrimSpace(r.URL.Query().Get("err")) {
		case "timezone":
			errMsg = s.t(r, "error.timeZoneInvalid")
		case "masterKeyCurrent":
			errMsg = s.t(r, "error.masterKeyCurrentMismatch")
		case "masterKeyPassword":
			errMsg = s.t(r, "error.masterKeyPasswordInvalid")
		case "masterKeyConfirm":
			errMsg = s.t(r, "error.masterKeyConfirmMismatch")
		case "masterKeyNotSet":
			errMsg = s.t(r, "error.masterKeyNotSet")
		case "masterKeyNew":
			errMsg = s.t(r, "error.masterKeyNewInvalid")
		case "masterKeyDecrypt":
			errMsg = s.t(r, "error.masterKeyDecryptFailed")
		case "masterKeyRotate":
			errMsg = s.t(r, "error.masterKeyRotateFailed")
		}
	}

	cfg, err := s.config.GetDecryptedConfig()
	if err != nil {
		s.render(w, r, systemData{
			layoutData: layoutData{
				Title:        title,
				BodyTemplate: "system",
				User:         st.User,
				HasUsers:     st.HasUsers,
				Error:        err.Error(),
			},
		})
		return
	}

	usersCount, _ := storage.CountUsers(s.db)

	versionsCount := int64(0)
	_ = s.db.QueryRowContext(r.Context(), "SELECT COUNT(1) FROM config_versions").Scan(&versionsCount)

	sessionsCount := int64(0)
	_ = s.db.QueryRowContext(r.Context(), "SELECT COUNT(1) FROM sessions").Scan(&sessionsCount)

	dbPath := ""
	dbSize := ""
	if p, size, err := getSQLiteMainDBPathAndSize(r.Context(), s.db); err == nil {
		dbPath = p
		dbSize = formatBytes(size)
	}

	buildVersion := "devel"
	if info, ok := debug.ReadBuildInfo(); ok && info != nil && info.Main.Version != "" {
		buildVersion = info.Main.Version
	}

	now := time.Now()
	gitRewriteData := s.buildGitHTMLRewriteData(r, cfg)
	rewriteRuntimeData := s.buildRewriteRuntimeData(r, cfg)
	ports := cfg.Ports
	if ports.Sakuya == 0 {
		ports.Sakuya = 3200
	}
	if ports.Patchouli == 0 {
		ports.Patchouli = 3201
	}

	s.render(w, r, systemData{
		layoutData: layoutData{
			Title:        title,
			BodyTemplate: "system",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Notice:       notice,
			Error:        errMsg,
		},

		GoVersion:         runtime.Version(),
		BuildVersion:      buildVersion,
		Uptime:            time.Since(s.startedAt).Truncate(time.Second).String(),
		StartedAt:         s.startedAt.Format(time.RFC3339),
		Now:               now.Format(time.RFC3339),
		SessionTTLSeconds: s.sessionTTL,

		EncryptionEnabled: s.config.IsEncryptionEnabled(),
		ConfigUpdatedAt:   s.config.GetUpdatedAt(),

		DBPath:        defaultString(dbPath, "(unknown)"),
		DBSize:        defaultString(dbSize, "(unknown)"),
		UsersCount:    usersCount,
		VersionsCount: versionsCount,
		SessionsCount: sessionsCount,

		Ports: ports,

		AdminStatus: checkServiceStatusWithCookie(r.Context(), s.port, sessionCookieValue(r)),
		TorcherinoStatus: func() serviceStatus {
			if cfg.Torcherino.Disabled {
				return disabledServiceStatus(cfg.Ports.Torcherino)
			}
			return checkServiceStatus(r.Context(), cfg.Ports.Torcherino)
		}(),
		CdnjsStatus: func() serviceStatus {
			if cfg.Cdnjs.Disabled {
				return disabledServiceStatus(cfg.Ports.Cdnjs)
			}
			return checkServiceStatus(r.Context(), cfg.Ports.Cdnjs)
		}(),
		GitStatus: func() serviceStatus {
			if cfg.Git.Disabled {
				return disabledServiceStatus(cfg.Ports.Git)
			}
			return checkServiceStatus(r.Context(), cfg.Ports.Git)
		}(),
		SakuyaOplistStatus: func() serviceStatus {
			port := ports.Sakuya
			if port == 0 {
				port = 3200
			}
			if cfg.Sakuya.Disabled {
				return disabledServiceStatus(port)
			}
			if !cfg.Sakuya.Oplist.Disabled &&
				strings.TrimSpace(cfg.Sakuya.Oplist.Address) != "" &&
				strings.TrimSpace(cfg.Sakuya.Oplist.Token) != "" {
				return checkServiceStatus(r.Context(), port)
			}
			for _, it := range cfg.Sakuya.Instances {
				if it.Disabled {
					continue
				}
				if strings.TrimSpace(it.Prefix) == "" || strings.TrimSpace(it.Address) == "" || strings.TrimSpace(it.Token) == "" {
					continue
				}
				return checkServiceStatus(r.Context(), port)
			}
			return disabledServiceStatus(port)
		}(),
		PatchouliStatus: func() serviceStatus {
			port := ports.Patchouli
			if port == 0 {
				port = 3201
			}
			if cfg.Patchouli.Disabled || strings.TrimSpace(cfg.Patchouli.Repo) == "" {
				return disabledServiceStatus(port)
			}
			return checkServiceStatus(r.Context(), port)
		}(),

		Redis: checkRedisStatus(r.Context(), cfg.Cdnjs.Redis.Host, cfg.Cdnjs.Redis.Port),

		GitHTMLRewrite: gitRewriteData,
		RewriteRuntime: rewriteRuntimeData,
	})
}

func (s *server) systemGitRewriteStatus(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// continue
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, err := s.config.GetDecryptedConfig()
	if err != nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}

	payload := map[string]any{
		"ok":             true,
		"time":           time.Now().UTC().Format(time.RFC3339Nano),
		"gitHTMLRewrite": s.buildGitHTMLRewriteData(r, cfg),
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

func (s *server) systemRewriteRuntimeStatus(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// continue
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, err := s.config.GetDecryptedConfig()
	if err != nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}

	payload := map[string]any{
		"ok":             true,
		"time":           time.Now().UTC().Format(time.RFC3339Nano),
		"rewriteRuntime": s.buildRewriteRuntimeData(r, cfg),
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

func (s *server) systemRewriteTrace(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// continue
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, err := s.config.GetDecryptedConfig()
	if err != nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}

	payload := map[string]any{
		"ok":             true,
		"time":           time.Now().UTC().Format(time.RFC3339Nano),
		"rewriteRuntime": s.buildRewriteRuntimeData(r, cfg),
		"memoryStatus":   rewritebudget.CurrentMemoryStatus(),
		"governor":       rewritebudget.CurrentGovernorStatus(),
		"model":          rewritebudget.CurrentRuntimeModelStatus(),
		"trace":          rewritebudget.ExportRuntimeTrace(),
	}
	b, _ := json.MarshalIndent(payload, "", "  ")

	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	if strings.TrimSpace(r.URL.Query().Get("download")) == "1" {
		filename := "hazuki-rewrite-trace-" + time.Now().UTC().Format("20060102T150405Z") + ".json"
		w.Header().Set("content-disposition", "attachment; filename=\""+filename+"\"")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}

func (s *server) buildGitHTMLRewriteData(r *http.Request, cfg model.AppConfig) systemGitHTMLRewriteData {
	status := gitproxy.CurrentHTMLRewriteStatus()
	unknownLength := "-"
	if status.UnknownLengthStreams {
		unknownLength = s.t(r, "system.gitRewrite.unknownLengthValue")
	}
	samplesText := strconv.FormatInt(status.BufferedSamples, 10) + " / " + strconv.FormatInt(status.StreamingSamples, 10)
	return systemGitHTMLRewriteData{
		Enabled:            hasAnyEnabledGitProxy(cfg),
		BudgetSource:       localizeRewriteBudgetSource(s, r, status.BudgetSource),
		MemoryBudget:       formatOptionalBytes(status.MemoryBudgetBytes),
		GoUsed:             formatOptionalBytes(status.GoMemoryUsedBytes),
		EffectiveUsed:      formatOptionalBytes(status.EffectiveUsedBytes),
		CgroupCurrent:      formatOptionalBytes(status.CgroupCurrentBytes),
		CgroupEvents:       formatCgroupEvents(status.CgroupHighEvents, status.CgroupMaxEvents, status.CgroupOOMEvents, status.CgroupOOMKillEvents),
		Reserve:            formatOptionalBytes(status.RewriteReserveBytes),
		Headroom:           formatOptionalBytes(status.HeadroomBytes),
		BufferedLimit:      formatOptionalBytes(status.BufferedLimitBytes),
		StreamChunk:        formatOptionalBytes(int64(status.StreamChunkBytes)),
		UnknownLength:      unknownLength,
		ActiveRewrites:     formatOptionalCount(status.ActiveRewrites),
		CalibrationSamples: samplesText,
		RecentHTMLP90:      formatOptionalBytes(status.RecentHTMLP90Bytes),
		BufferedCostFactor: formatOptionalMilliRatio(status.BufferedCostMultiplierMilli, "x"),
		UsableShare:        formatOptionalMilliPercent(status.UsableShareMilli),
		BufferedSpeed:      formatOptionalRate(status.BufferedThroughputBytesPerSec),
		StreamSpeed:        formatOptionalRate(status.StreamingThroughputBytesPerSec),
	}
}

func (s *server) buildRewriteRuntimeData(r *http.Request, cfg model.AppConfig) systemRewriteRuntimeData {
	shared := rewritebudget.CurrentMemoryStatus()
	governor := rewritebudget.CurrentGovernorStatus()
	model := rewritebudget.CurrentRuntimeModelStatus()
	sharedActiveRewrites := rewritebudget.CurrentActiveCount()
	gitStatus := gitproxy.CurrentHTMLRewriteStatus()
	torcherinoStatus := torcherinoproxy.CurrentRewriteStatus()

	return systemRewriteRuntimeData{
		Shared: systemRewriteRuntimeSharedData{
			BudgetSource:          localizeRewriteBudgetSource(s, r, shared.BudgetSource),
			MemoryBudget:          formatOptionalBytes(shared.MemoryBudgetBytes),
			GoUsed:                formatOptionalBytes(shared.GoMemoryUsedBytes),
			EffectiveUsed:         formatOptionalBytes(shared.EffectiveUsedBytes),
			CgroupCurrent:         formatOptionalBytes(shared.CgroupCurrentBytes),
			CgroupEvents:          formatCgroupEvents(shared.CgroupHighEvents, shared.CgroupMaxEvents, shared.CgroupOOMEvents, shared.CgroupOOMKillEvents),
			ActiveRewrites:        formatOptionalCount(sharedActiveRewrites),
			ActiveRewriteWeight:   formatBytesOrZero(shared.ActiveRewriteWeightBytes),
			BufferedAdmission:     formatBufferedAdmission(shared.BufferedAdmissionInUseBytes, shared.BufferedAdmissionLimitBytes),
			BufferedFallbacks:     formatOptionalCount(shared.BufferedAdmissionFallbacks),
			AdaptivePressure:      formatMilliPercentZeroAllowed(governor.PressureMilli),
			AdaptiveTraceSamples:  formatOptionalCount(shared.AdaptiveTraceSamples),
			PredictedUtilization:  formatMilliPercentZeroAllowed(shared.PredictedUtilMilli),
			LearnedAdmissionShare: formatMilliPercentZeroAllowed(shared.LearnedAdmissionShareMilli),
			AdaptiveGC:            formatGovernorGC(governor),
			AdaptiveMode:          localizeGovernorMode(s, r, governor.Mode),
			AutoTuneStatus:        formatAutoTuneStatus(s, r, model),
			AutoTuneWindow:        formatAutoTuneWindow(model),
			AutoTuneGain:          formatAutoTuneGain(model),
			ActiveAdmissionModel:  formatAdmissionModel(model.ActiveAdmissionIntercept, model.ActiveAdmissionSlope),
			AutoTuneLast:          formatAutoTuneLast(model),
		},
		GitHTML:        s.buildRewriteTunerData(r, hasAnyEnabledGitProxy(cfg), gitStatus.ActiveRewrites, gitStatus.RewriteReserveBytes, gitStatus.HeadroomBytes, gitStatus.BufferedLimitBytes, gitStatus.StreamChunkBytes, gitStatus.UnknownLengthStreams, gitStatus.BufferedSamples, gitStatus.StreamingSamples, gitStatus.RecentHTMLP90Bytes, gitStatus.BufferedCostMultiplierMilli, gitStatus.UsableShareMilli, gitStatus.BufferedThroughputBytesPerSec, gitStatus.StreamingThroughputBytesPerSec),
		TorcherinoHTML: s.buildRewriteTunerData(r, !cfg.Torcherino.Disabled, torcherinoStatus.HTML.ActiveRewrites, torcherinoStatus.HTML.RewriteReserveBytes, torcherinoStatus.HTML.HeadroomBytes, torcherinoStatus.HTML.BufferedLimitBytes, torcherinoStatus.HTML.StreamChunkBytes, torcherinoStatus.HTML.UnknownLengthStreams, torcherinoStatus.HTML.BufferedSamples, torcherinoStatus.HTML.StreamingSamples, torcherinoStatus.HTML.RecentBodyP90Bytes, torcherinoStatus.HTML.BufferedCostMultiplierMilli, torcherinoStatus.HTML.UsableShareMilli, torcherinoStatus.HTML.BufferedThroughputBytesPerSec, torcherinoStatus.HTML.StreamingThroughputBytesPerSec),
		TorcherinoJSON: s.buildRewriteTunerData(r, !cfg.Torcherino.Disabled, torcherinoStatus.JSON.ActiveRewrites, torcherinoStatus.JSON.RewriteReserveBytes, torcherinoStatus.JSON.HeadroomBytes, torcherinoStatus.JSON.BufferedLimitBytes, torcherinoStatus.JSON.StreamChunkBytes, torcherinoStatus.JSON.UnknownLengthStreams, torcherinoStatus.JSON.BufferedSamples, torcherinoStatus.JSON.StreamingSamples, torcherinoStatus.JSON.RecentBodyP90Bytes, torcherinoStatus.JSON.BufferedCostMultiplierMilli, torcherinoStatus.JSON.UsableShareMilli, torcherinoStatus.JSON.BufferedThroughputBytesPerSec, torcherinoStatus.JSON.StreamingThroughputBytesPerSec),
	}
}

func (s *server) buildRewriteTunerData(r *http.Request, enabled bool, activeRewrites, reserveBytes, headroomBytes, bufferedLimitBytes int64, streamChunkBytes int, unknownLengthStreams bool, bufferedSamples, streamingSamples, recentBodyP90Bytes int64, bufferedCostMilli, usableShareMilli int, bufferedThroughputBytesPerSec, streamingThroughputBytesPerSec int64) systemRewriteRuntimeTunerData {
	unknownLength := "-"
	if unknownLengthStreams {
		unknownLength = s.t(r, "system.rewriteRuntime.unknownLengthValue")
	}
	samplesText := strconv.FormatInt(bufferedSamples, 10) + " / " + strconv.FormatInt(streamingSamples, 10)
	return systemRewriteRuntimeTunerData{
		Enabled:            enabled,
		ActiveRewrites:     formatOptionalCount(activeRewrites),
		Reserve:            formatOptionalBytes(reserveBytes),
		Headroom:           formatOptionalBytes(headroomBytes),
		BufferedLimit:      formatOptionalBytes(bufferedLimitBytes),
		StreamChunk:        formatOptionalBytes(int64(streamChunkBytes)),
		UnknownLength:      unknownLength,
		CalibrationSamples: samplesText,
		RecentBodyP90:      formatOptionalBytes(recentBodyP90Bytes),
		BufferedCostFactor: formatOptionalMilliRatio(bufferedCostMilli, "x"),
		UsableShare:        formatOptionalMilliPercent(usableShareMilli),
		BufferedSpeed:      formatOptionalRate(bufferedThroughputBytesPerSec),
		StreamSpeed:        formatOptionalRate(streamingThroughputBytesPerSec),
	}
}

func localizeRewriteBudgetSource(s *server, r *http.Request, source string) string {
	switch source {
	case "gomemlimit":
		return s.t(r, "system.gitRewrite.source.gomemlimit")
	case "cgroup":
		return s.t(r, "system.gitRewrite.source.cgroup")
	case "memavailable":
		return s.t(r, "system.gitRewrite.source.memavailable")
	default:
		return s.t(r, "system.gitRewrite.source.default")
	}
}

func localizeGovernorMode(s *server, r *http.Request, mode string) string {
	switch mode {
	case "relaxed":
		return s.t(r, "system.rewriteRuntime.gcMode.relaxed")
	case "balanced":
		return s.t(r, "system.rewriteRuntime.gcMode.balanced")
	case "guarded":
		return s.t(r, "system.rewriteRuntime.gcMode.guarded")
	case "emergency":
		return s.t(r, "system.rewriteRuntime.gcMode.emergency")
	default:
		return s.t(r, "system.rewriteRuntime.gcMode.off")
	}
}

func localizeAutoTuneReason(s *server, r *http.Request, reason string) string {
	switch reason {
	case "disabled":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.disabled")
	case "warming_up":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.warming")
	case "promote":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.promote")
	case "no_material_change":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.steady")
	case "validation_improvement_too_small":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.waiting")
	case "validation_risk_regressed":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.risk")
	case "context_regression_too_large":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.context")
	case "insufficient_validation_observations":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.notEnoughObs")
	case "insufficient_validation_contexts":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.notEnoughCtx")
	case "boot":
		return s.t(r, "system.rewriteRuntime.autoTune.reason.boot")
	default:
		if strings.TrimSpace(reason) == "" {
			return s.t(r, "system.rewriteRuntime.autoTune.reason.waiting")
		}
		return reason
	}
}

func formatOptionalBytes(n int64) string {
	if n <= 0 {
		return "-"
	}
	return formatBytes(n)
}

func formatOptionalRate(n int64) string {
	if n <= 0 {
		return "-"
	}
	return formatBytes(n) + "/s"
}

func formatBytesOrZero(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	return formatBytes(n)
}

func formatBufferedAdmission(inUseBytes, limitBytes int64) string {
	if limitBytes <= 0 {
		return "-"
	}
	return formatBytesOrZero(inUseBytes) + " / " + formatBytes(limitBytes)
}

func formatOptionalCount(n int64) string {
	if n <= 0 {
		return "0"
	}
	return strconv.FormatInt(n, 10)
}

func formatOptionalMilliPercent(milli int) string {
	if milli <= 0 {
		return "-"
	}
	return strconv.FormatFloat(float64(milli)/10, 'f', 1, 64) + "%"
}

func formatMilliPercentZeroAllowed(milli int) string {
	if milli < 0 {
		return "-"
	}
	return strconv.FormatFloat(float64(milli)/10, 'f', 1, 64) + "%"
}

func formatOptionalMilliRatio(milli int, suffix string) string {
	if milli <= 0 {
		return "-"
	}
	return strconv.FormatFloat(float64(milli)/1000, 'f', 3, 64) + suffix
}

func formatSignedPercent(value float64) string {
	return strconv.FormatFloat(value, 'f', 1, 64) + "%"
}

func formatAutoTuneStatus(s *server, r *http.Request, status rewritebudget.RuntimeModelStatus) string {
	if !status.AutoTuneEnabled {
		return s.t(r, "system.rewriteRuntime.autoTune.off")
	}
	return s.t(r, "system.rewriteRuntime.autoTune.on") + " · " + localizeAutoTuneReason(s, r, status.AutoTuneReason)
}

func formatAutoTuneWindow(status rewritebudget.RuntimeModelStatus) string {
	traceSamples := status.AutoTuneTraceSamples
	if traceSamples < 0 {
		traceSamples = 0
	}
	observationSamples := status.AutoTuneObservationSamples
	if observationSamples < 0 {
		observationSamples = 0
	}
	window := strconv.Itoa(traceSamples) + " / " + strconv.Itoa(observationSamples)
	if status.AutoTuneIntervalSeconds > 0 {
		return window + " / " + (time.Duration(status.AutoTuneIntervalSeconds) * time.Second).String()
	}
	return window
}

func formatAutoTuneGain(status rewritebudget.RuntimeModelStatus) string {
	if status.AutoTuneObservationSamples <= 0 {
		return "-"
	}
	return formatSignedPercent(status.AutoTuneTrainImprovementPct) + " / " +
		formatSignedPercent(status.AutoTuneValidationImprovementPct)
}

func formatAdmissionModel(intercept, slope float64) string {
	if intercept <= 0 {
		return "-"
	}
	return "i=" + strconv.FormatFloat(intercept, 'f', 3, 64) + " / s=" + strconv.FormatFloat(slope, 'f', 3, 64)
}

func formatAutoTuneLast(status rewritebudget.RuntimeModelStatus) string {
	if status.AutoTuneLastRunAt.IsZero() && status.AutoTuneLastPromotedAt.IsZero() {
		return "-"
	}
	runText := "-"
	if !status.AutoTuneLastRunAt.IsZero() {
		runText = status.AutoTuneLastRunAt.UTC().Format("15:04:05")
	}
	promoteText := "-"
	if !status.AutoTuneLastPromotedAt.IsZero() {
		promoteText = status.AutoTuneLastPromotedAt.UTC().Format("15:04:05")
	}
	return runText + " / " + promoteText
}

func formatCgroupEvents(high, maxv, oom, oomKill int64) string {
	if high <= 0 && maxv <= 0 && oom <= 0 && oomKill <= 0 {
		return "0 / 0 / 0 / 0"
	}
	return strconv.FormatInt(high, 10) + " / " +
		strconv.FormatInt(maxv, 10) + " / " +
		strconv.FormatInt(oom, 10) + " / " +
		strconv.FormatInt(oomKill, 10)
}

func formatGovernorGC(status rewritebudget.GovernorStatus) string {
	if !status.Enabled || status.CurrentGCPercent <= 0 {
		return "-"
	}
	if status.MinGCPercent > 0 && status.MaxGCPercent >= status.MinGCPercent {
		return strconv.Itoa(status.CurrentGCPercent) + " [" + strconv.Itoa(status.MinGCPercent) + "-" + strconv.Itoa(status.MaxGCPercent) + "]"
	}
	return strconv.Itoa(status.CurrentGCPercent)
}

func hasAnyEnabledGitProxy(cfg model.AppConfig) bool {
	if !cfg.Git.Disabled {
		return true
	}
	for _, inst := range cfg.GitInstances {
		if !inst.Git.Disabled {
			return true
		}
	}
	return false
}

func (s *server) systemTimeZone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	spec, ok := normalizeAdminTimeZoneSpec(r.FormValue("timeZone"))
	if !ok {
		http.Redirect(w, r, "/system?err=timezone", http.StatusFound)
		return
	}
	if err := storage.SetAdminTimeZone(r.Context(), s.db, spec); err != nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/system?ok=1", http.StatusFound)
}

func (s *server) systemMasterKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	st := getState(r.Context())
	if st == nil || st.User == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/system?err=masterKeyRotate", http.StatusFound)
		return
	}

	currentMasterKey := strings.TrimSpace(r.FormValue("currentMasterKey"))
	newMasterKey := strings.TrimSpace(r.FormValue("newMasterKey"))
	confirmNewMasterKey := strings.TrimSpace(r.FormValue("confirmNewMasterKey"))
	password := r.FormValue("password")

	currentEnvKey := strings.TrimSpace(os.Getenv("HAZUKI_MASTER_KEY"))
	if currentMasterKey != currentEnvKey {
		http.Redirect(w, r, "/system?err=masterKeyCurrent", http.StatusFound)
		return
	}

	_, ok, err := storage.VerifyUserPassword(s.db, st.User.Username, password)
	if err != nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}
	if !ok {
		http.Redirect(w, r, "/system?err=masterKeyPassword", http.StatusFound)
		return
	}

	if newMasterKey == "" || newMasterKey == currentEnvKey {
		http.Redirect(w, r, "/system?err=masterKeyNew", http.StatusFound)
		return
	}
	if newMasterKey != confirmNewMasterKey {
		http.Redirect(w, r, "/system?err=masterKeyConfirm", http.StatusFound)
		return
	}

	if err := s.config.RotateMasterKey(currentMasterKey, newMasterKey); err != nil {
		if errors.Is(err, storage.ErrDecryptAuthFailed) || strings.Contains(strings.ToLower(err.Error()), "decrypt") {
			http.Redirect(w, r, "/system?err=masterKeyDecrypt", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/system?err=masterKeyRotate", http.StatusFound)
		return
	}

	_ = os.Setenv("HAZUKI_MASTER_KEY", newMasterKey)
	envUpdated, _ := syncMasterKeyToDotEnv(newMasterKey)
	if envUpdated {
		http.Redirect(w, r, "/system?ok=masterKey&env=1", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/system?ok=masterKey&env=0", http.StatusFound)
}

func (s *server) systemMasterKeyVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	st := getState(r.Context())
	if st == nil || st.User == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/system?err=masterKeyRotate", http.StatusFound)
		return
	}

	currentMasterKey := strings.TrimSpace(r.FormValue("currentMasterKey"))
	password := r.FormValue("password")

	currentEnvKey := strings.TrimSpace(os.Getenv("HAZUKI_MASTER_KEY"))
	if currentEnvKey == "" {
		http.Redirect(w, r, "/system?err=masterKeyNotSet", http.StatusFound)
		return
	}
	if currentMasterKey != currentEnvKey {
		http.Redirect(w, r, "/system?err=masterKeyCurrent", http.StatusFound)
		return
	}

	_, ok, err := storage.VerifyUserPassword(s.db, st.User.Username, password)
	if err != nil {
		http.Error(w, "Bad gateway", http.StatusBadGateway)
		return
	}
	if !ok {
		http.Redirect(w, r, "/system?err=masterKeyPassword", http.StatusFound)
		return
	}

	http.Redirect(w, r, "/system?ok=masterKeyVerify", http.StatusFound)
}

func (s *server) redisCache(w http.ResponseWriter, r *http.Request) {
	st := getState(r.Context())
	title := s.t(r, "page.redisCache.title")

	switch r.Method {
	case http.MethodGet:
		// continue
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	notice := ""
	if ok := strings.TrimSpace(r.URL.Query().Get("ok")); ok != "" {
		switch ok {
		case "cleared":
			n := strings.TrimSpace(r.URL.Query().Get("n"))
			if n == "" {
				n = "0"
			}
			notice = s.t(r, "redisCache.clearedNotice", n)
		case "deleted":
			notice = s.t(r, "redisCache.deletedNotice")
		default:
			notice = s.t(r, "common.saved")
		}
	}
	errMsg := ""
	if errQ := strings.TrimSpace(r.URL.Query().Get("err")); errQ != "" {
		switch errQ {
		case "redis":
			errMsg = s.t(r, "redisCache.errorRedis")
		default:
			errMsg = s.t(r, "error.badRequest")
		}
	}

	cfg, err := s.config.GetDecryptedConfig()
	if err != nil {
		s.render(w, r, redisCacheData{
			layoutData: layoutData{
				Title:        title,
				BodyTemplate: "redis_cache",
				User:         st.User,
				HasUsers:     st.HasUsers,
				Notice:       notice,
				Error:        err.Error(),
			},
			Namespace:     rediscache.Cdnjs.Key,
			MarkerKey:     rediscache.MarkerKey,
			MarkerValue:   rediscache.MarkerValue,
			IndexKey:      rediscache.Cdnjs.IndexKey,
			Limit:         100,
			Page:          1,
			ClearableDesc: rediscache.Cdnjs.Prefix + "*",
		})
		return
	}

	nsKey := strings.TrimSpace(r.URL.Query().Get("ns"))
	ns, ok := rediscache.NamespaceByKey(nsKey)
	if !ok {
		ns = rediscache.Cdnjs
	}

	redisSt := checkRedisStatus(r.Context(), cfg.Cdnjs.Redis.Host, cfg.Cdnjs.Redis.Port)
	data := redisCacheData{
		layoutData: layoutData{
			Title:        title,
			BodyTemplate: "redis_cache",
			User:         st.User,
			HasUsers:     st.HasUsers,
			Notice:       notice,
			Error:        errMsg,
		},
		Namespace:     ns.Key,
		Redis:         redisSt,
		MarkerKey:     rediscache.MarkerKey,
		MarkerValue:   rediscache.MarkerValue,
		IndexKey:      ns.IndexKey,
		Limit:         100,
		Page:          1,
		ClearableDesc: ns.Prefix + "*",
	}

	if redisSt.Status != "ok" {
		s.render(w, r, data)
		return
	}

	limit := 100
	if v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit"))); err == nil && v > 0 {
		limit = v
	}
	if limit > 500 {
		limit = 500
	}
	page := 1
	if v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page"))); err == nil && v > 0 {
		page = v
	}

	client, _, ok := newRedisAdminClient(cfg.Cdnjs.Redis.Host, cfg.Cdnjs.Redis.Port)
	if !ok || client == nil {
		data.Redis = redisStatus{Status: "disabled"}
		s.render(w, r, data)
		return
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	marker, err := client.Get(ctx, rediscache.MarkerKey).Result()
	if err == nil && strings.TrimSpace(marker) != "" {
		data.MarkerPresent = true
	}

	total, err := client.ZCard(ctx, ns.IndexKey).Result()
	if err != nil {
		data.Error = s.t(r, "redisCache.errorRedis")
		s.render(w, r, data)
		return
	}
	data.TrackedCount = total
	data.Page = page
	data.Limit = limit

	start := int64((page - 1) * limit)
	stop := start + int64(limit) - 1
	ids, err := client.ZRevRange(ctx, ns.IndexKey, start, stop).Result()
	if err != nil {
		data.Error = s.t(r, "redisCache.errorRedis")
		s.render(w, r, data)
		return
	}

	pipe := client.Pipeline()
	metaCmds := make([]*redis.MapStringStringCmd, 0, len(ids))
	ttlCmds := make([]*redis.DurationCmd, 0, len(ids))
	for _, id := range ids {
		metaCmds = append(metaCmds, pipe.HGetAll(ctx, rediscache.MetaKey(ns, id)))
		ttlCmds = append(ttlCmds, pipe.TTL(ctx, rediscache.BodyKey(ns, id)))
	}
	_, _ = pipe.Exec(ctx)

	entries := make([]redisCacheEntry, 0, len(ids))
	stale := make([]any, 0, len(ids))
	for i, id := range ids {
		meta, _ := metaCmds[i].Result()
		if len(meta) == 0 {
			stale = append(stale, id)
			continue
		}

		url := strings.TrimSpace(meta["url"])
		typ := strings.TrimSpace(meta["type"])
		sizeBytes := int64(0)
		if v, err := strconv.ParseInt(strings.TrimSpace(meta["size"]), 10, 64); err == nil && v > 0 {
			sizeBytes = v
		}
		sizeHuman := ""
		if sizeBytes > 0 {
			sizeHuman = formatBytes(sizeBytes)
		}
		updatedAt := ""
		if v, err := strconv.ParseInt(strings.TrimSpace(meta["updatedAt"]), 10, 64); err == nil && v > 0 {
			updatedAt = time.Unix(v, 0).UTC().Format(time.RFC3339)
		}

		ttl := int64(0)
		if d, err := ttlCmds[i].Result(); err == nil {
			ttl = int64(d.Seconds())
		}

		entries = append(entries, redisCacheEntry{
			ID:         id,
			URL:        url,
			Type:       typ,
			SizeBytes:  sizeBytes,
			SizeHuman:  sizeHuman,
			UpdatedAt:  updatedAt,
			TTLSeconds: ttl,
		})
	}
	data.Entries = entries

	data.HasPrev = page > 1
	data.HasNext = int64(page*limit) < total
	data.PrevPage = page - 1
	data.NextPage = page + 1

	// Best-effort: prune stale index entries (expired keys).
	if len(stale) > 0 {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, _ = client.ZRem(ctx2, ns.IndexKey, stale...).Result()
		cancel2()
	}

	s.render(w, r, data)
}

func (s *server) redisCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/system/redis-cache?err=bad", http.StatusFound)
		return
	}
	nsKey := strings.TrimSpace(r.FormValue("ns"))
	ns, ok := rediscache.NamespaceByKey(nsKey)
	if !ok {
		ns = rediscache.Cdnjs
	}

	cfg, err := s.config.GetDecryptedConfig()
	if err != nil {
		http.Redirect(w, r, "/system/redis-cache?ns="+url.QueryEscape(ns.Key)+"&err=redis", http.StatusFound)
		return
	}

	client, _, ok := newRedisAdminClient(cfg.Cdnjs.Redis.Host, cfg.Cdnjs.Redis.Port)
	if !ok || client == nil {
		http.Redirect(w, r, "/system/redis-cache?ns="+url.QueryEscape(ns.Key)+"&err=redis", http.StatusFound)
		return
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var deleted int64
	cursor := uint64(0)
	for {
		keys, next, err := client.Scan(ctx, cursor, ns.Prefix+"*", 500).Result()
		if err != nil {
			http.Redirect(w, r, "/system/redis-cache?ns="+url.QueryEscape(ns.Key)+"&err=redis", http.StatusFound)
			return
		}
		if len(keys) > 0 {
			n, _ := client.Del(ctx, keys...).Result()
			deleted += n
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	http.Redirect(w, r, "/system/redis-cache?ns="+url.QueryEscape(ns.Key)+"&ok=cleared&n="+url.QueryEscape(strconv.FormatInt(deleted, 10)), http.StatusFound)
}

func (s *server) redisCacheDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/system/redis-cache?err=bad", http.StatusFound)
		return
	}
	nsKey := strings.TrimSpace(r.FormValue("ns"))
	ns, ok := rediscache.NamespaceByKey(nsKey)
	if !ok {
		ns = rediscache.Cdnjs
	}
	pageRaw := strings.TrimSpace(r.FormValue("page"))
	limitRaw := strings.TrimSpace(r.FormValue("limit"))

	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Redirect(w, r, "/system/redis-cache?ns="+url.QueryEscape(ns.Key)+"&err=bad", http.StatusFound)
		return
	}

	cfg, err := s.config.GetDecryptedConfig()
	if err != nil {
		http.Redirect(w, r, "/system/redis-cache?ns="+url.QueryEscape(ns.Key)+"&err=redis", http.StatusFound)
		return
	}

	client, _, ok := newRedisAdminClient(cfg.Cdnjs.Redis.Host, cfg.Cdnjs.Redis.Port)
	if !ok || client == nil {
		http.Redirect(w, r, "/system/redis-cache?ns="+url.QueryEscape(ns.Key)+"&err=redis", http.StatusFound)
		return
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	_, _ = client.Del(ctx, rediscache.BodyKey(ns, id), rediscache.TypeKey(ns, id), rediscache.MetaKey(ns, id)).Result()
	_, _ = client.ZRem(ctx, ns.IndexKey, id).Result()

	redirectURL := "/system/redis-cache?ns=" + url.QueryEscape(ns.Key) + "&ok=deleted"
	if pageRaw != "" {
		redirectURL += "&page=" + url.QueryEscape(pageRaw)
	}
	if limitRaw != "" {
		redirectURL += "&limit=" + url.QueryEscape(limitRaw)
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}
