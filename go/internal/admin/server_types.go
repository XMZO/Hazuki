package admin

import (
	"html/template"

	"hazuki-go/internal/model"
	"hazuki-go/internal/storage"
)

type reqState struct {
	HasUsers bool
	User     *storage.User
}

type ctxKey string

const stateKey ctxKey = "hazuki_admin_state"

type layoutData struct {
	Title        string
	BodyTemplate string
	User         *storage.User
	HasUsers     bool
	Notice       string
	Error        string

	Lang   string
	JSI18n template.JS

	// Admin panel display timezone. Values: auto | UTC | +HH:MM / -HH:MM
	TimeZone string
}

type dashboardData struct {
	layoutData
	UpdatedAt string
	Ports     model.PortsConfig
	AdminURL  string

	AdminStatus serviceStatus

	TorcherinoURL       string
	TorcherinoHealthURL string
	TorcherinoStatus    serviceStatus

	CdnjsURL       string
	CdnjsHealthURL string
	CdnjsStatus    serviceStatus

	GitURL       string
	GitHealthURL string
	GitStatus    serviceStatus
	GitInstances []gitInstanceRow

	SakuyaURL       string
	SakuyaHealthURL string
	SakuyaStatus    serviceStatus

	PatchouliURL       string
	PatchouliHealthURL string
	PatchouliStatus    serviceStatus

	CdnjsRedis redisStatus

	Warnings []string
}

type cdnjsData struct {
	layoutData
	Cdnjs             model.CdnjsConfig
	CdnjsPort         int
	CdnjsPortValue    string
	GhUserPolicyValue string
	AllowedUsersCsv   string
	BlockedUsersCsv   string
	RedisPortValue    string
	DefaultTTLValue   string
	TTLOverridesValue string
	TTLEffectiveJSON  template.JS
	CdnjsBaseURL      string
	CdnjsHealthURL    string

	RedisStatus redisStatus
	CdnjsStatus serviceStatus
}

type gitData struct {
	layoutData
	Git               model.GitConfig
	GitPort           int
	GitPortValue      string
	GitPortKey        string
	GitEnabled        bool
	TokenIsSet        bool
	AuthScheme        string
	BlockedRegionsCsv string
	BlockedIPsCsv     string
	ReplaceDictJson   string
	GitBaseURL        string
	GitHealthURL      string
	GitStatus         serviceStatus

	CurrentInstanceID   string
	CurrentInstanceName string
	Instances           []gitInstanceRow
}

type gitInstanceRow struct {
	ID        string
	Name      string
	Port      int
	Enabled   bool
	BaseURL   string
	HealthURL string
	Status    serviceStatus
}

type torcherinoData struct {
	layoutData
	Torcherino model.TorcherinoConfig

	TorcherinoPort      int
	TorcherinoPortValue string

	TorcherinoClientInfo torcherinoClientInfo

	DefaultTargetValue string
	HostMappingJSON    string

	SecretIsSet                    bool
	WorkerSecretKeyValue           string
	WorkerSecretHeadersCsvValue    string
	WorkerSecretHeaderMapJSONValue string

	RedisCacheEnabled                bool
	RedisCacheMaxBodyBytesValue      string
	RedisCacheDefaultTTLSecondsValue string
	RedisCacheMaxTTLSecondsValue     string

	TorcherinoBaseURL   string
	TorcherinoHealthURL string
	TorcherinoStatus    serviceStatus
}

type torcherinoClientInfo struct {
	ClientIP        string
	RemoteAddr      string
	CfConnectingIP  string
	XForwardedFor   string
	XRealIP         string
	XHazukiClientIP string
}

type sakuyaOplistData struct {
	layoutData
	Sakuya model.SakuyaConfig

	SakuyaPort      int
	SakuyaPortValue string

	CurrentInstanceID     string
	CurrentInstanceName   string
	CurrentInstancePrefix string

	OplistPrefixValue    string
	OplistAddressValue   string
	OplistPublicURLValue string

	TokenIsSet bool
	TokenValue string

	SakuyaEnabled  bool
	ServiceEnabled bool

	IgnoreDuplicatePrefix bool

	Instances []sakuyaInstanceRow

	SakuyaBaseURL   string
	SakuyaHealthURL string
	SakuyaStatus    serviceStatus
}

type sakuyaInstanceRow struct {
	ID         string
	Name       string
	Prefix     string
	Enabled    bool
	Address    string
	ServiceURL string
	ExampleURL string
}

type patchouliData struct {
	layoutData
	Patchouli model.PatchouliConfig

	PatchouliPort      int
	PatchouliPortValue string

	PatchouliEnabled bool
	TokenIsSet       bool
	AccessKeyIsSet   bool

	KindValue                      string
	AllowedRedirectHostSuffixesCsv string

	PatchouliBaseURL   string
	PatchouliHealthURL string
	PatchouliStatus    serviceStatus
}

type accountData struct {
	layoutData
}

type versionsData struct {
	layoutData
	Versions []storage.ConfigVersion
}

type exportData struct {
	layoutData
	MasterKeyIsSet bool
}

type importData struct {
	layoutData
}

type systemData struct {
	layoutData

	GoVersion         string
	BuildVersion      string
	Uptime            string
	StartedAt         string
	Now               string
	SessionTTLSeconds int
	EncryptionEnabled bool
	ConfigUpdatedAt   string

	DBPath        string
	DBSize        string
	UsersCount    int
	VersionsCount int64
	SessionsCount int64

	Ports model.PortsConfig

	AdminStatus        serviceStatus
	TorcherinoStatus   serviceStatus
	CdnjsStatus        serviceStatus
	GitStatus          serviceStatus
	SakuyaOplistStatus serviceStatus
	PatchouliStatus    serviceStatus

	Redis redisStatus

	GitHTMLRewrite systemGitHTMLRewriteData
	RewriteRuntime systemRewriteRuntimeData
}

type systemGitHTMLRewriteData struct {
	Enabled            bool   `json:"enabled"`
	BudgetSource       string `json:"budgetSource"`
	MemoryBudget       string `json:"memoryBudget"`
	GoUsed             string `json:"goUsed"`
	EffectiveUsed      string `json:"effectiveUsed"`
	CgroupCurrent      string `json:"cgroupCurrent"`
	CgroupEvents       string `json:"cgroupEvents"`
	Reserve            string `json:"reserve"`
	Headroom           string `json:"headroom"`
	BufferedLimit      string `json:"bufferedLimit"`
	StreamChunk        string `json:"streamChunk"`
	UnknownLength      string `json:"unknownLength"`
	ActiveRewrites     string `json:"activeRewrites"`
	CalibrationSamples string `json:"calibrationSamples"`
	RecentHTMLP90      string `json:"recentHTMLP90"`
	BufferedCostFactor string `json:"bufferedCostFactor"`
	UsableShare        string `json:"usableShare"`
	BufferedSpeed      string `json:"bufferedSpeed"`
	StreamSpeed        string `json:"streamSpeed"`
}

type systemRewriteRuntimeData struct {
	Shared         systemRewriteRuntimeSharedData `json:"shared"`
	GitHTML        systemRewriteRuntimeTunerData  `json:"gitHTML"`
	TorcherinoHTML systemRewriteRuntimeTunerData  `json:"torcherinoHTML"`
	TorcherinoJSON systemRewriteRuntimeTunerData  `json:"torcherinoJSON"`
}

type systemRewriteRuntimeSharedData struct {
	BudgetSource          string `json:"budgetSource"`
	MemoryBudget          string `json:"memoryBudget"`
	GoUsed                string `json:"goUsed"`
	EffectiveUsed         string `json:"effectiveUsed"`
	CgroupCurrent         string `json:"cgroupCurrent"`
	CgroupEvents          string `json:"cgroupEvents"`
	ActiveRewrites        string `json:"activeRewrites"`
	ActiveRewriteWeight   string `json:"activeRewriteWeight"`
	BufferedAdmission     string `json:"bufferedAdmission"`
	BufferedFallbacks     string `json:"bufferedFallbacks"`
	AdaptivePressure      string `json:"adaptivePressure"`
	AdaptiveTraceSamples  string `json:"adaptiveTraceSamples"`
	PredictedUtilization  string `json:"predictedUtilization"`
	LearnedAdmissionShare string `json:"learnedAdmissionShare"`
	AdaptiveGC            string `json:"adaptiveGC"`
	AdaptiveMode          string `json:"adaptiveMode"`
}

type systemRewriteRuntimeTunerData struct {
	Enabled            bool   `json:"enabled"`
	ActiveRewrites     string `json:"activeRewrites"`
	Reserve            string `json:"reserve"`
	Headroom           string `json:"headroom"`
	BufferedLimit      string `json:"bufferedLimit"`
	StreamChunk        string `json:"streamChunk"`
	UnknownLength      string `json:"unknownLength"`
	CalibrationSamples string `json:"calibrationSamples"`
	RecentBodyP90      string `json:"recentBodyP90"`
	BufferedCostFactor string `json:"bufferedCostFactor"`
	UsableShare        string `json:"usableShare"`
	BufferedSpeed      string `json:"bufferedSpeed"`
	StreamSpeed        string `json:"streamSpeed"`
}

type redisCacheEntry struct {
	ID         string
	URL        string
	Type       string
	SizeBytes  int64
	SizeHuman  string
	UpdatedAt  string
	TTLSeconds int64
}

type redisCacheData struct {
	layoutData

	Redis redisStatus

	Namespace string

	MarkerKey     string
	MarkerValue   string
	MarkerPresent bool

	IndexKey      string
	TrackedCount  int64
	Page          int
	Limit         int
	Entries       []redisCacheEntry
	HasPrev       bool
	HasNext       bool
	PrevPage      int
	NextPage      int
	ClearableDesc string
}

type trafficData struct {
	layoutData
	Retention    storage.TrafficRetention
	GitInstances []trafficGitInstanceOption
}

type trafficGitInstanceOption struct {
	ID    string
	Name  string
	Value string // "git:<id>"
}

type wizardData struct {
	layoutData

	TokenIsSet  bool
	SecretIsSet bool

	TorcherinoDefaultTarget         string
	TorcherinoHostMappingJSON       string
	TorcherinoWorkerSecretKey       string
	TorcherinoWorkerSecretHeaders   string
	TorcherinoWorkerSecretHeaderMap string

	CdnjsDefaultGhUser  string
	CdnjsAllowedGhUsers string
	CdnjsGhUserPolicy   string
	CdnjsBlockedGhUsers string
	CdnjsAssetURL       string
	CdnjsRedisHost      string
	CdnjsRedisPort      string

	GitUpstreamPath string
	GitGithubToken  string
}

type redisStatus struct {
	Addr      string `json:"addr"`
	Status    string `json:"status"` // ok | error | disabled
	LatencyMS int64  `json:"latencyMS"`
	Error     string `json:"error,omitempty"`

	ServerVersion    string `json:"serverVersion,omitempty"`
	UptimeSeconds    int64  `json:"uptimeSeconds,omitempty"`
	ConnectedClients int64  `json:"connectedClients,omitempty"`
	UsedMemoryHuman  string `json:"usedMemoryHuman,omitempty"`
	DBSize           int64  `json:"dbSize,omitempty"`

	KeyspaceHits   int64 `json:"keyspaceHits,omitempty"`
	KeyspaceMisses int64 `json:"keyspaceMisses,omitempty"`
}

type serviceStatus struct {
	Addr      string
	URL       string
	Service   string
	Status    string // ok | error | disabled
	LatencyMS int64
	Error     string
}
