package analyzer

import "time"

type Report struct {
	Application         string              `json:"application"`
	Namespace           string              `json:"namespace"`
	GeneratedAt         time.Time           `json:"generatedAt"`
	ClusterCapabilities ClusterCapabilities `json:"clusterCapabilities"`
	Workloads           []WorkloadReport    `json:"workloads"`
	SharedSignals       []SignalReport      `json:"sharedSignals"`
	Summary             Summary             `json:"summary"`
	Proposal            *ProposalReport     `json:"proposal,omitempty"`
}

type ClusterCapabilities struct {
	InPlacePodResize InPlacePodResizeCapability `json:"inPlacePodResize"`
}

type InPlacePodResizeCapability struct {
	Supported   bool     `json:"supported"`
	Subresource string   `json:"subresource,omitempty"`
	Verbs       []string `json:"verbs,omitempty"`
	NormalMode  string   `json:"normalMode"`
	Reason      string   `json:"reason,omitempty"`
}

type Summary struct {
	WorkloadsTotal   int `json:"workloadsTotal"`
	CommitBlocked    int `json:"commitBlocked"`
	MetricsHealthy   int `json:"metricsHealthy"`
	MetricsDegraded  int `json:"metricsDegraded"`
	MetricsUnhealthy int `json:"metricsUnhealthy"`
}

type WorkloadReport struct {
	Name             string             `json:"name"`
	Namespace        string             `json:"namespace"`
	Kind             string             `json:"kind"`
	Deployment       string             `json:"deployment"`
	Replicas         int32              `json:"replicas"`
	ReadyReplicas    int32              `json:"readyReplicas"`
	FleetManaged     bool               `json:"fleetManaged"`
	FleetObjectSet   string             `json:"fleetObjectSet,omitempty"`
	HelmRelease      string             `json:"helmRelease,omitempty"`
	Scaling          ScalingReport      `json:"scaling"`
	Containers       []ContainerReport  `json:"containers"`
	Autoscalers      []Autoscaler       `json:"autoscalers,omitempty"`
	PDBs             []PDBReport        `json:"pdbs,omitempty"`
	Availability     AvailabilityReport `json:"availability"`
	Rollout          RolloutReport      `json:"rollout"`
	CommitBlocked    bool               `json:"commitBlocked"`
	BlockReasons     []string           `json:"blockReasons,omitempty"`
	MetricProfile    string             `json:"metricProfile"`
	MetricSignals    []SignalReport     `json:"metricSignals"`
	MetricsCondition string             `json:"metricsCondition"`
	Recommendation   Recommendation     `json:"recommendation"`
}

type ScalingReport struct {
	Replicas bool `json:"replicas"`
	CPU      bool `json:"cpu"`
	Memory   bool `json:"memory"`
}

type ContainerReport struct {
	Name               string  `json:"name"`
	CPURequest         string  `json:"cpuRequest,omitempty"`
	CPURequestCores    float64 `json:"cpuRequestCores,omitempty"`
	MemoryRequest      string  `json:"memoryRequest,omitempty"`
	MemoryRequestBytes float64 `json:"memoryRequestBytes,omitempty"`
	CPULimit           string  `json:"cpuLimit,omitempty"`
	MemoryLimit        string  `json:"memoryLimit,omitempty"`
}

type Autoscaler struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	APIVersion string `json:"apiVersion"`
}

type PDBReport struct {
	Name                   string `json:"name"`
	MinAvailable           string `json:"minAvailable,omitempty"`
	MaxUnavailable         string `json:"maxUnavailable,omitempty"`
	DisruptionsAllowed     int32  `json:"disruptionsAllowed"`
	DesiredHealthy         int32  `json:"desiredHealthy"`
	CurrentHealthy         int32  `json:"currentHealthy"`
	ExpectedPods           int32  `json:"expectedPods"`
	MinimumReplicaFloor    int32  `json:"minimumReplicaFloor,omitempty"`
	ScaleDownFloorEnforced bool   `json:"scaleDownFloorEnforced"`
}

type AvailabilityReport struct {
	ReplicaFloor                 int32    `json:"replicaFloor"`
	Public                       bool     `json:"public"`
	Services                     []string `json:"services,omitempty"`
	ReadyEndpoints               int32    `json:"readyEndpoints"`
	ReadyNodes                   int      `json:"readyNodes"`
	RollingUpdateZeroUnavailable bool     `json:"rollingUpdateZeroUnavailable"`
	Reasons                      []string `json:"reasons,omitempty"`
}

type RolloutReport struct {
	Evaluated           bool     `json:"evaluated"`
	Settled             bool     `json:"settled"`
	Generation          int64    `json:"generation"`
	ObservedGeneration  int64    `json:"observedGeneration"`
	DesiredReplicas     int32    `json:"desiredReplicas"`
	UpdatedReplicas     int32    `json:"updatedReplicas"`
	ReadyReplicas       int32    `json:"readyReplicas"`
	AvailableReplicas   int32    `json:"availableReplicas"`
	UnavailableReplicas int32    `json:"unavailableReplicas"`
	TerminatingPods     int      `json:"terminatingPods"`
	PendingPods         int      `json:"pendingPods"`
	IncompleteInitPods  int      `json:"incompleteInitPods"`
	UnreadyPods         int      `json:"unreadyPods"`
	Reasons             []string `json:"reasons,omitempty"`
}

type SignalReport struct {
	Name         string          `json:"name"`
	Required     bool            `json:"required"`
	Healthy      bool            `json:"healthy"`
	Anomaly      AnomalyStatus   `json:"anomaly"`
	Series       int             `json:"series"`
	Sample       *float64        `json:"sample,omitempty"`
	Query        string          `json:"query"`
	Error        string          `json:"error,omitempty"`
	ResultType   string          `json:"resultType,omitempty"`
	History      *SignalHistory  `json:"history,omitempty"`
	HistoryError string          `json:"historyError,omitempty"`
	Forecast     *SignalForecast `json:"forecast,omitempty"`
}

type AnomalyStatus struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type SignalHistory struct {
	Window         string     `json:"window"`
	Step           string     `json:"step"`
	Points         int        `json:"points"`
	ExpectedPoints int        `json:"expectedPoints,omitempty"`
	Coverage       float64    `json:"coverage,omitempty"`
	FirstSampleAt  *time.Time `json:"firstSampleAt,omitempty"`
	LastSampleAt   *time.Time `json:"lastSampleAt,omitempty"`
	Min            float64    `json:"min"`
	P50            float64    `json:"p50"`
	P95            float64    `json:"p95"`
	Max            float64    `json:"max"`
}

type SignalForecast struct {
	TrendSlopePerHour float64            `json:"trendSlopePerHour"`
	Horizons          []ForecastHorizon  `json:"horizons,omitempty"`
	Baselines         []ForecastBaseline `json:"baselines,omitempty"`
	Reason            string             `json:"reason,omitempty"`
}

type ForecastHorizon struct {
	Horizon     string  `json:"horizon"`
	Forecast    float64 `json:"forecast"`
	P95BandLow  float64 `json:"p95BandLow"`
	P95BandHigh float64 `json:"p95BandHigh"`
	Confidence  float64 `json:"confidence"`
}

type ForecastBaseline struct {
	Name   string  `json:"name"`
	Window string  `json:"window"`
	Points int     `json:"points"`
	P50    float64 `json:"p50"`
	P95    float64 `json:"p95"`
	Max    float64 `json:"max"`
}

type Recommendation struct {
	Mode                     string                   `json:"mode"`
	CurrentReplicas          int32                    `json:"currentReplicas"`
	RecommendedReplicas      int32                    `json:"recommendedReplicas"`
	CurrentCPURequest        string                   `json:"currentCpuRequest,omitempty"`
	RecommendedCPURequest    string                   `json:"recommendedCpuRequest,omitempty"`
	CurrentMemoryRequest     string                   `json:"currentMemoryRequest,omitempty"`
	RecommendedMemoryRequest string                   `json:"recommendedMemoryRequest,omitempty"`
	Confidence               float64                  `json:"confidence"`
	ConfidenceAssessment     ConfidenceAssessment     `json:"confidenceAssessment"`
	Safety                   SafetyAssessment         `json:"safety"`
	Learning                 LearningEvidence         `json:"learning"`
	ReplicaDecision          *ReplicaDecision         `json:"replicaDecision,omitempty"`
	ReasonCodes              []string                 `json:"reasonCodes,omitempty"`
	Blocked                  bool                     `json:"blocked"`
	BlockReasons             []string                 `json:"blockReasons,omitempty"`
	PatchPlan                *PatchPlan               `json:"patchPlan,omitempty"`
	Stability                *RecommendationStability `json:"stability,omitempty"`
}

type ConfidenceAssessment struct {
	Base              float64                  `json:"base"`
	Adjusted          float64                  `json:"adjusted"`
	Decay             float64                  `json:"decay"`
	MinAutoCommit     float64                  `json:"minAutoCommit"`
	AutoCommitAllowed bool                     `json:"autoCommitAllowed"`
	Reasons           []string                 `json:"reasons,omitempty"`
	Signals           []SignalConfidenceFactor `json:"signals,omitempty"`
}

type SignalConfidenceFactor struct {
	Name     string  `json:"name"`
	Required bool    `json:"required"`
	Quality  string  `json:"quality"`
	Decay    float64 `json:"decay"`
	Reason   string  `json:"reason"`
}

type SafetyAssessment struct {
	Classification    string         `json:"classification"`
	AutoCommitAllowed bool           `json:"autoCommitAllowed"`
	Reasons           []string       `json:"reasons,omitempty"`
	Factors           []SafetyFactor `json:"factors,omitempty"`
}

type SafetyFactor struct {
	Name           string `json:"name"`
	Classification string `json:"classification"`
	Reason         string `json:"reason"`
}

type RecommendationStability struct {
	Actionable bool          `json:"actionable"`
	Replicas   StabilityGate `json:"replicas"`
	CPU        StabilityGate `json:"cpu"`
	Memory     StabilityGate `json:"memory"`
}

type StabilityGate struct {
	Status   string `json:"status"`
	Observed int    `json:"observed"`
	Required int    `json:"required"`
	Reason   string `json:"reason,omitempty"`
}

type ReplicaDecision struct {
	RecommendedReplicas int32                      `json:"recommendedReplicas"`
	Score               float64                    `json:"score"`
	Basis               string                     `json:"basis"`
	Floor               int32                      `json:"floor"`
	FloorReasons        []string                   `json:"floorReasons,omitempty"`
	Components          []ReplicaDecisionComponent `json:"components,omitempty"`
}

type ReplicaDecisionComponent struct {
	Name      string  `json:"name"`
	Score     float64 `json:"score"`
	Replicas  int32   `json:"replicas,omitempty"`
	Basis     string  `json:"basis,omitempty"`
	Observed  string  `json:"observed,omitempty"`
	Influence string  `json:"influence,omitempty"`
}

type LearningEvidence struct {
	Mode        string              `json:"mode"`
	Description string              `json:"description"`
	Persistent  *PersistentLearning `json:"persistent,omitempty"`
	Signals     []LearnedSignal     `json:"signals,omitempty"`
	Decisions   []LearnedDecision   `json:"decisions,omitempty"`
}

type PersistentLearning struct {
	Enabled                      bool                   `json:"enabled"`
	PriorRecommendationRuns      int                    `json:"priorRecommendationRuns"`
	PriorSignalObservations      int                    `json:"priorSignalObservations"`
	ForecastAccuracy             *ForecastAccuracy      `json:"forecastAccuracy,omitempty"`
	Seasonality                  *SeasonalityLearning   `json:"seasonality,omitempty"`
	LastObservedAt               *time.Time             `json:"lastObservedAt,omitempty"`
	LastRecommendedReplicas      int32                  `json:"lastRecommendedReplicas,omitempty"`
	LastRecommendedCPURequest    string                 `json:"lastRecommendedCpuRequest,omitempty"`
	LastRecommendedMemoryRequest string                 `json:"lastRecommendedMemoryRequest,omitempty"`
	LastOutcome                  *RecommendationOutcome `json:"lastOutcome,omitempty"`
	Message                      string                 `json:"message"`
}

type SeasonalityLearning struct {
	Enabled              bool                  `json:"enabled"`
	ObservationCount     int                   `json:"observationCount"`
	CurrentHour          int                   `json:"currentHour"`
	CurrentDayOfWeek     int                   `json:"currentDayOfWeek"`
	CurrentDayType       string                `json:"currentDayType"`
	CurrentTrafficBand   string                `json:"currentTrafficBand,omitempty"`
	Signals              []SeasonalSignal      `json:"signals,omitempty"`
	LatencyByTrafficBand []SeasonalLatencyBand `json:"latencyByTrafficBand,omitempty"`
	Message              string                `json:"message"`
}

type SeasonalSignal struct {
	Signal    string  `json:"signal"`
	Bucket    string  `json:"bucket"`
	Hour      int     `json:"hour"`
	DayOfWeek int     `json:"dayOfWeek,omitempty"`
	DayType   string  `json:"dayType,omitempty"`
	Points    int     `json:"points"`
	P50       float64 `json:"p50"`
	P95       float64 `json:"p95"`
	Max       float64 `json:"max"`
}

type SeasonalLatencyBand struct {
	TrafficBand string  `json:"trafficBand"`
	Hour        int     `json:"hour"`
	DayType     string  `json:"dayType"`
	Points      int     `json:"points"`
	P50         float64 `json:"p50"`
	P95         float64 `json:"p95"`
	Max         float64 `json:"max"`
}

type ForecastAccuracy struct {
	Enabled                       bool                    `json:"enabled"`
	Samples                       int                     `json:"samples"`
	Signals                       []ForecastAccuracyScore `json:"signals,omitempty"`
	ConfidenceAdjustment          float64                 `json:"confidenceAdjustment"`
	WasteReductionBias            string                  `json:"wasteReductionBias,omitempty"`
	Message                       string                  `json:"message"`
	LastScoredRecommendationCount int                     `json:"lastScoredRecommendationCount,omitempty"`
}

type ForecastAccuracyScore struct {
	Signal                   string  `json:"signal"`
	Samples                  int     `json:"samples"`
	MeanAbsolutePercentError float64 `json:"meanAbsolutePercentError"`
	MeanBiasPercent          float64 `json:"meanBiasPercent"`
	Classification           string  `json:"classification"`
}

type RecommendationOutcome struct {
	Status                           string     `json:"status"`
	PreviousObservedAt               *time.Time `json:"previousObservedAt,omitempty"`
	PreviousCurrentReplicas          int32      `json:"previousCurrentReplicas"`
	PreviousRecommendedReplicas      int32      `json:"previousRecommendedReplicas"`
	PreviousCurrentCPURequest        string     `json:"previousCurrentCpuRequest,omitempty"`
	PreviousRecommendedCPURequest    string     `json:"previousRecommendedCpuRequest,omitempty"`
	PreviousCurrentMemoryRequest     string     `json:"previousCurrentMemoryRequest,omitempty"`
	PreviousRecommendedMemoryRequest string     `json:"previousRecommendedMemoryRequest,omitempty"`
	CurrentReplicas                  int32      `json:"currentReplicas"`
	CurrentCPURequest                string     `json:"currentCpuRequest,omitempty"`
	CurrentMemoryRequest             string     `json:"currentMemoryRequest,omitempty"`
	Details                          []string   `json:"details,omitempty"`
}

type LearnedSignal struct {
	Name           string  `json:"name"`
	Window         string  `json:"window,omitempty"`
	Step           string  `json:"step,omitempty"`
	Points         int     `json:"points"`
	Current        float64 `json:"current,omitempty"`
	P50            float64 `json:"p50,omitempty"`
	P95            float64 `json:"p95,omitempty"`
	Max            float64 `json:"max,omitempty"`
	CurrentVsP95   float64 `json:"currentVsP95,omitempty"`
	CurrentVsMax   float64 `json:"currentVsMax,omitempty"`
	Classification string  `json:"classification"`
}

type LearnedDecision struct {
	Subject    string `json:"subject"`
	Learned    string `json:"learned"`
	Observed   string `json:"observed"`
	Conclusion string `json:"conclusion"`
}

type PatchPlan struct {
	SourceFile   string        `json:"sourceFile"`
	Resource     string        `json:"resource"`
	Needed       bool          `json:"needed"`
	Blocked      bool          `json:"blocked"`
	BlockReasons []string      `json:"blockReasons,omitempty"`
	Changes      []PatchChange `json:"changes,omitempty"`
	Diff         string        `json:"diff,omitempty"`
	Errors       []string      `json:"errors,omitempty"`
}

type PatchChange struct {
	Field       string `json:"field"`
	Operation   string `json:"operation"`
	Current     string `json:"current"`
	Recommended string `json:"recommended"`
}

type ProposalReport struct {
	Mode         string         `json:"mode"`
	Kind         string         `json:"kind"`
	Needed       bool           `json:"needed"`
	Blocked      bool           `json:"blocked"`
	Pushed       bool           `json:"pushed"`
	PatchFile    string         `json:"patchFile,omitempty"`
	Branch       string         `json:"branch,omitempty"`
	Commit       string         `json:"commit,omitempty"`
	Remote       string         `json:"remote,omitempty"`
	PushRef      string         `json:"pushRef,omitempty"`
	Message      string         `json:"message,omitempty"`
	Files        []ProposalFile `json:"files,omitempty"`
	BlockReasons []string       `json:"blockReasons,omitempty"`
	Errors       []string       `json:"errors,omitempty"`
}

type ProposalFile struct {
	SourceFile      string        `json:"sourceFile"`
	Diff            string        `json:"diff,omitempty"`
	Changes         []PatchChange `json:"changes,omitempty"`
	ProposedContent string        `json:"-"`
}

type ProposalStatusReport struct {
	Worktree              string   `json:"worktree"`
	CurrentBranch         string   `json:"currentBranch"`
	ProposalBranches      []string `json:"proposalBranches,omitempty"`
	BaseBranch            string   `json:"baseBranch"`
	Upstream              string   `json:"upstream,omitempty"`
	HasUpstream           bool     `json:"hasUpstream"`
	BranchDiffersFromBase bool     `json:"branchDiffersFromBase"`
	LatestProposalCommit  string   `json:"latestProposalCommit,omitempty"`
	LatestProposalSubject string   `json:"latestProposalSubject,omitempty"`
	LatestProposalFiles   []string `json:"latestProposalFiles,omitempty"`
	DirtyLines            []string `json:"dirtyLines,omitempty"`
	PatchArtifacts        []string `json:"patchArtifacts,omitempty"`
	Errors                []string `json:"errors,omitempty"`
}

type ObservationReport struct {
	Application string                `json:"application"`
	Namespace   string                `json:"namespace"`
	GeneratedAt time.Time             `json:"generatedAt"`
	Git         GitObservation        `json:"git"`
	Workloads   []WorkloadObservation `json:"workloads"`
	Summary     ObservationSummary    `json:"summary"`
	Errors      []string              `json:"errors,omitempty"`
}

type AutoRollbackReport struct {
	Application string             `json:"application"`
	Namespace   string             `json:"namespace"`
	GeneratedAt time.Time          `json:"generatedAt"`
	Needed      bool               `json:"needed"`
	Blocked     bool               `json:"blocked"`
	Reasons     []string           `json:"reasons,omitempty"`
	Errors      []string           `json:"errors,omitempty"`
	Observation *ObservationReport `json:"observation,omitempty"`
	Rollback    *ProposalReport    `json:"rollback,omitempty"`
}

type GitObservation struct {
	Worktree              string   `json:"worktree"`
	Branch                string   `json:"branch"`
	BaseBranch            string   `json:"baseBranch"`
	Upstream              string   `json:"upstream,omitempty"`
	HasUpstream           bool     `json:"hasUpstream"`
	BranchDiffersFromBase bool     `json:"branchDiffersFromBase"`
	LatestProposalCommit  string   `json:"latestProposalCommit,omitempty"`
	LatestProposalSubject string   `json:"latestProposalSubject,omitempty"`
	DirtyLines            []string `json:"dirtyLines,omitempty"`
}

type ObservationSummary struct {
	WorkloadsTotal int `json:"workloadsTotal"`
	Applied        int `json:"applied"`
	Pending        int `json:"pending"`
	Drifted        int `json:"drifted"`
	Failed         int `json:"failed"`
	Unknown        int `json:"unknown"`
	Improved       int `json:"improved"`
	Neutral        int `json:"neutral"`
	Regressed      int `json:"regressed"`
	Unsafe         int `json:"unsafe"`
}

type WorkloadObservation struct {
	Name             string             `json:"name"`
	Namespace        string             `json:"namespace"`
	Deployment       string             `json:"deployment"`
	Resource         string             `json:"resource"`
	SourceFile       string             `json:"sourceFile,omitempty"`
	MetricsCondition string             `json:"metricsCondition"`
	Status           string             `json:"status"`
	Outcome          string             `json:"outcome"`
	Desired          ObservedResources  `json:"desired"`
	Live             ObservedResources  `json:"live"`
	Fields           []FieldObservation `json:"fields,omitempty"`
	Reasons          []string           `json:"reasons,omitempty"`
	Errors           []string           `json:"errors,omitempty"`
}

type ObservedResources struct {
	Replicas      string `json:"replicas,omitempty"`
	CPURequest    string `json:"cpuRequest,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
}

type FieldObservation struct {
	Field   string `json:"field"`
	Desired string `json:"desired"`
	Live    string `json:"live"`
	Match   bool   `json:"match"`
	Managed bool   `json:"managed"`
}
