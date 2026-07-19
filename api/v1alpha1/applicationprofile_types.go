package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ApplicationProfileSpec defines one independently reconciled application profile.
type ApplicationProfileSpec struct {
	// Namespace is the namespace containing all target workloads.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Git identifies the Fleet source repository for proposed changes.
	// +optional
	Git GitSpec `json:"git,omitempty"`

	// SharedSignals are application-level signals reused by workload decisions.
	// +optional
	SharedSignals []SharedSignal `json:"sharedSignals,omitempty"`

	// Workloads contains the independently analyzed workload targets.
	// +kubebuilder:validation:MinItems=1
	Workloads []WorkloadSpec `json:"workloads"`

	// MetricProfiles contains named Prometheus query templates referenced by workloads and shared signals.
	// +kubebuilder:validation:MinProperties=1
	MetricProfiles map[string]MetricProfile `json:"metricProfiles"`

	// ReconcileInterval overrides the controller's default interval for this profile.
	// +optional
	ReconcileInterval *metav1.Duration `json:"reconcileInterval,omitempty"`

	// Suspend stops reconciliation without deleting the profile or its status.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// StateKey overrides the namespace/name identity used for persisted learning state.
	// Set this only when migrating an existing file-based profile database.
	// +kubebuilder:validation:MinLength=1
	// +optional
	StateKey string `json:"stateKey,omitempty"`
}

type GitSpec struct {
	// +optional
	Provider string `json:"provider,omitempty"`
	// +optional
	Mode string `json:"mode,omitempty"`
	// +optional
	Repository string `json:"repository,omitempty"`
	// +optional
	Branch string `json:"branch,omitempty"`
	// +optional
	BasePath string `json:"basePath,omitempty"`
}

type SharedSignal struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	MetricProfileRef string `json:"metricProfileRef"`
	// +optional
	Required bool `json:"required,omitempty"`
	// +optional
	Vars map[string]string `json:"vars,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.helmValues) || (has(self.sourceFile) && size(self.sourceFile) > 0)",message="sourceFile is required when helmValues is configured"
// +kubebuilder:validation:XValidation:rule="!has(self.helmValues) || !has(self.scaling) || !has(self.scaling.replicas) || !self.scaling.replicas || has(self.helmValues.paths.replicas)",message="helmValues.paths.replicas is required when replica scaling is enabled"
// +kubebuilder:validation:XValidation:rule="!has(self.helmValues) || !has(self.scaling) || !has(self.scaling.cpu) || !self.scaling.cpu || has(self.helmValues.paths.cpuRequest)",message="helmValues.paths.cpuRequest is required when CPU scaling is enabled"
// +kubebuilder:validation:XValidation:rule="!has(self.helmValues) || !has(self.scaling) || !has(self.scaling.memory) || !self.scaling.memory || has(self.helmValues.paths.memoryRequest)",message="helmValues.paths.memoryRequest is required when memory scaling is enabled"
type WorkloadSpec struct {
	// +kubebuilder:validation:MinLength=1
	Name      string    `json:"name"`
	TargetRef TargetRef `json:"targetRef"`
	// +optional
	SourceFile string `json:"sourceFile,omitempty"`
	// HelmValues maps recommendation fields to existing scalar keys in a Helm values file.
	// When set, SourceFile is treated as Helm values instead of a rendered Kubernetes manifest.
	// +optional
	HelmValues *HelmValuesSpec `json:"helmValues,omitempty"`
	// +kubebuilder:validation:MinLength=1
	MetricProfileRef string `json:"metricProfileRef"`
	// +optional
	Scaling ScalingSpec `json:"scaling,omitempty"`
	// +optional
	Bounds BoundsSpec `json:"bounds,omitempty"`
	// +optional
	Policy PolicySpec `json:"policy,omitempty"`
	// +optional
	Vars map[string]string `json:"vars,omitempty"`
}

// HelmValuesSpec configures fields managed in a Helm values file.
type HelmValuesSpec struct {
	// Paths maps recommendation fields to ordered YAML mapping keys.
	Paths HelmValuePaths `json:"paths"`
}

// HelmValuePaths identifies the ordered mapping keys for fields managed in a Helm values file.
// Each path must point to an existing scalar so a misspelled chart key cannot be silently added.
// +kubebuilder:validation:MinProperties=1
type HelmValuePaths struct {
	// Replicas is the path to the chart value that renders the target workload spec.replicas.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +optional
	Replicas []string `json:"replicas,omitempty"`
	// CPURequest is the path to the chart value that renders the target container CPU request.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +optional
	CPURequest []string `json:"cpuRequest,omitempty"`
	// MemoryRequest is the path to the chart value that renders the target container memory request.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +optional
	MemoryRequest []string `json:"memoryRequest,omitempty"`
}

type TargetRef struct {
	// +optional
	// +kubebuilder:default="apps/v1"
	APIVersion string `json:"apiVersion,omitempty"`
	// +kubebuilder:validation:Enum=Deployment;StatefulSet
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type ScalingSpec struct {
	// +optional
	Replicas bool `json:"replicas,omitempty"`
	// +optional
	CPU bool `json:"cpu,omitempty"`
	// +optional
	Memory bool `json:"memory,omitempty"`
}

type BoundsSpec struct {
	// +optional
	Replicas ReplicaBounds `json:"replicas,omitempty"`
	// +optional
	CPU ChangeBounds `json:"cpu,omitempty"`
	// +optional
	Memory ChangeBounds `json:"memory,omitempty"`
}

type ReplicaBounds struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	Min int `json:"min,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Max int `json:"max,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxIncreasePercent int `json:"maxIncreasePercent,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxDecreasePercent int `json:"maxDecreasePercent,omitempty"`
}

type ChangeBounds struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxIncreasePercent int `json:"maxIncreasePercent,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxDecreasePercent int `json:"maxDecreasePercent,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinChangePercent float64 `json:"minChangePercent,omitempty"`
}

type PolicySpec struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxProposalsPerHour int `json:"maxProposalsPerHour,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxProposalsPerDay int `json:"maxProposalsPerDay,omitempty"`
	// +optional
	Safety SafetyPolicySpec `json:"safety,omitempty"`
	// +optional
	Confidence ConfidencePolicySpec `json:"confidence,omitempty"`
	// +optional
	AvailabilityRecovery AvailabilityRecoveryPolicySpec `json:"availabilityRecovery,omitempty"`
}

type SafetyPolicySpec struct {
	// +optional
	// +kubebuilder:validation:items:Enum=low_risk;medium_risk;high_risk
	AllowAutoCommit []string `json:"allowAutoCommit,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=low_risk;medium_risk;high_risk
	MaxDecreaseRisk string `json:"maxDecreaseRisk,omitempty"`
	// +optional
	UrgentBypassAllowed *bool `json:"urgentBypassAllowed,omitempty"`
}

type ConfidencePolicySpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	MinAutoCommit float64 `json:"minAutoCommit,omitempty"`
}

type AvailabilityRecoveryPolicySpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +optional
	FailureGracePeriod *metav1.Duration `json:"failureGracePeriod,omitempty"`
	// +optional
	Cooldown *metav1.Duration `json:"cooldown,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxAttemptsPerHour int `json:"maxAttemptsPerHour,omitempty"`
}

type MetricProfile struct {
	// +optional
	Description string `json:"description,omitempty"`
	// +kubebuilder:validation:MinProperties=1
	Signals map[string]Signal `json:"signals"`
}

type Signal struct {
	// +kubebuilder:validation:MinLength=1
	Query string `json:"query"`
	// +optional
	Required bool `json:"required,omitempty"`
}

// ApplicationProfileStatus is a bounded summary of the most recent reconciliation.
type ApplicationProfileStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	LastAttemptTime *metav1.Time `json:"lastAttemptTime,omitempty"`
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`
	// +optional
	NextReconcileTime *metav1.Time `json:"nextReconcileTime,omitempty"`
	// +optional
	Summary ApplicationProfileSummary `json:"summary,omitempty"`
	// +optional
	Workloads []WorkloadStatus `json:"workloads,omitempty"`
	// +optional
	Proposal *ProposalStatus `json:"proposal,omitempty"`
	// +optional
	Git *GitStatus `json:"git,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type ApplicationProfileSummary struct {
	// +optional
	Workloads int32 `json:"workloads,omitempty"`
	// +optional
	Healthy int32 `json:"healthy,omitempty"`
	// +optional
	Degraded int32 `json:"degraded,omitempty"`
	// +optional
	Unhealthy int32 `json:"unhealthy,omitempty"`
	// +optional
	Blocked int32 `json:"blocked,omitempty"`
	// +optional
	Emergencies int32 `json:"emergencies,omitempty"`
}

type WorkloadStatus struct {
	Name                string `json:"name"`
	Target              string `json:"target"`
	CurrentReplicas     int32  `json:"currentReplicas"`
	ReadyReplicas       int32  `json:"readyReplicas"`
	RecommendedReplicas int32  `json:"recommendedReplicas"`
	// +optional
	CurrentCPURequest string `json:"currentCpuRequest,omitempty"`
	// +optional
	RecommendedCPURequest string `json:"recommendedCpuRequest,omitempty"`
	// +optional
	CurrentMemoryRequest string `json:"currentMemoryRequest,omitempty"`
	// +optional
	RecommendedMemoryRequest string `json:"recommendedMemoryRequest,omitempty"`
	// +optional
	MetricsCondition string `json:"metricsCondition,omitempty"`
	// +optional
	Confidence float64 `json:"confidence,omitempty"`
	// +optional
	Safety string `json:"safety,omitempty"`
	// +optional
	Blocked bool `json:"blocked,omitempty"`
	// +optional
	Emergency bool `json:"emergency,omitempty"`
	// Patch summarizes Git source planning without storing the potentially large diff.
	// +optional
	Patch *WorkloadPatchStatus `json:"patch,omitempty"`
	// +optional
	Recovery *RecoveryStatus `json:"recovery,omitempty"`
}

type WorkloadPatchStatus struct {
	// +optional
	SourceFile string `json:"sourceFile,omitempty"`
	// +optional
	SourceFormat string `json:"sourceFormat,omitempty"`
	// +optional
	Needed bool `json:"needed,omitempty"`
	// +optional
	Blocked bool `json:"blocked,omitempty"`
	// +optional
	BlockReasons []string `json:"blockReasons,omitempty"`
	// +optional
	Errors []string `json:"errors,omitempty"`
}

type RecoveryStatus struct {
	// +optional
	Attempted bool `json:"attempted,omitempty"`
	// +optional
	Succeeded bool `json:"succeeded,omitempty"`
	// +optional
	Action string `json:"action,omitempty"`
	// +optional
	Pod string `json:"pod,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Error string `json:"error,omitempty"`
}

type ProposalStatus struct {
	// +optional
	Needed bool `json:"needed,omitempty"`
	// +optional
	Blocked bool `json:"blocked,omitempty"`
	// +optional
	Pushed bool `json:"pushed,omitempty"`
	// +optional
	Branch string `json:"branch,omitempty"`
	// +optional
	Commit string `json:"commit,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	BlockReasons []string `json:"blockReasons,omitempty"`
	// +optional
	Errors []string `json:"errors,omitempty"`
}

type GitStatus struct {
	// +optional
	Status string `json:"status,omitempty"`
	// +optional
	Branch string `json:"branch,omitempty"`
	// +optional
	Ahead int32 `json:"ahead,omitempty"`
	// +optional
	Behind int32 `json:"behind,omitempty"`
	// +optional
	Diverged bool `json:"diverged,omitempty"`
	// +optional
	Dirty bool `json:"dirty,omitempty"`
	// +optional
	Errors []string `json:"errors,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=aprof,categories=k8s-recommendation-engine
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Proposal",type=string,JSONPath=`.status.conditions[?(@.type=="ProposalReady")].status`
// +kubebuilder:printcolumn:name="Healthy",type=integer,JSONPath=`.status.summary.healthy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ApplicationProfile is the Schema for applicationprofiles.
type ApplicationProfile struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ApplicationProfileSpec `json:"spec"`
	// +optional
	Status ApplicationProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ApplicationProfileList contains a list of ApplicationProfile resources.
type ApplicationProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ApplicationProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ApplicationProfile{}, &ApplicationProfileList{})
}
