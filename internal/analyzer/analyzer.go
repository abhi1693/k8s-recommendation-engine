package analyzer

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"github.com/abhi1693/k8s-recommendation-engine/internal/kube"
	"github.com/abhi1693/k8s-recommendation-engine/internal/prom"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type Prometheus interface {
	Query(ctx context.Context, query string) (*prom.QueryResult, error)
	QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (*prom.RangeQueryResult, error)
}

type Analyzer struct {
	kube    *kube.Client
	prom    Prometheus
	options Options
}

type Options struct {
	HistoryWindow time.Duration
	HistoryStep   time.Duration
}

func New(kubeClient *kube.Client, promClient Prometheus, options ...Options) *Analyzer {
	opts := Options{
		HistoryWindow: 24 * time.Hour,
		HistoryStep:   5 * time.Minute,
	}
	if len(options) > 0 {
		opts = options[0]
		if opts.HistoryWindow <= 0 {
			opts.HistoryWindow = 24 * time.Hour
		}
		if opts.HistoryStep <= 0 {
			opts.HistoryStep = 5 * time.Minute
		}
	}
	return &Analyzer{kube: kubeClient, prom: promClient, options: opts}
}

func (a *Analyzer) Analyze(ctx context.Context, profile *config.ApplicationProfile) (*Report, error) {
	report := &Report{
		Application: profile.Metadata.Name,
		Namespace:   profile.Spec.Namespace,
		GeneratedAt: time.Now().UTC(),
	}
	report.ClusterCapabilities.InPlacePodResize = inPlacePodResizeCapability(a.kube.PodResizeCapability())

	for _, shared := range profile.Spec.SharedSignals {
		metricProfile := profile.MetricProfiles[shared.MetricProfileRef]
		vars := shared.VarsWithDefaults(profile.Spec.Namespace)
		for signalName, signal := range metricProfile.Signals {
			report.SharedSignals = append(report.SharedSignals, a.validateSignal(ctx, signalName, signal, vars))
		}
	}

	for _, workload := range profile.Spec.Workloads {
		workloadReport, err := a.analyzeWorkload(ctx, profile, workload, report.SharedSignals)
		if err != nil {
			return nil, err
		}
		report.Workloads = append(report.Workloads, workloadReport)
	}

	report.Summary.WorkloadsTotal = len(report.Workloads)
	for _, workload := range report.Workloads {
		if workload.CommitBlocked {
			report.Summary.CommitBlocked++
		}
		switch workload.MetricsCondition {
		case "healthy":
			report.Summary.MetricsHealthy++
		case "degraded":
			report.Summary.MetricsDegraded++
		default:
			report.Summary.MetricsUnhealthy++
		}
	}
	return report, nil
}

func inPlacePodResizeCapability(capability kube.PodResizeCapability) InPlacePodResizeCapability {
	return InPlacePodResizeCapability{
		Supported:   capability.Supported,
		Subresource: capability.Subresource,
		Verbs:       capability.Verbs,
		NormalMode:  "gitops",
		Reason:      capability.Reason,
	}
}

func (a *Analyzer) analyzeWorkload(ctx context.Context, profile *config.ApplicationProfile, workload config.WorkloadSpec, sharedSignals []SignalReport) (WorkloadReport, error) {
	deployment, err := a.kube.GetDeployment(ctx, profile.Spec.Namespace, workload.TargetRef.Name)
	if err != nil {
		return WorkloadReport{}, fmt.Errorf("get deployment %s/%s: %w", profile.Spec.Namespace, workload.TargetRef.Name, err)
	}

	report := WorkloadReport{
		Name:           workload.Name,
		Namespace:      profile.Spec.Namespace,
		Kind:           workload.TargetRef.Kind,
		Deployment:     workload.TargetRef.Name,
		Replicas:       int32(1),
		ReadyReplicas:  deployment.Status.ReadyReplicas,
		FleetManaged:   deployment.Annotations["objectset.rio.cattle.io/id"] != "",
		FleetObjectSet: deployment.Annotations["objectset.rio.cattle.io/id"],
		HelmRelease:    deployment.Annotations["meta.helm.sh/release-name"],
		Scaling: ScalingReport{
			Replicas: workload.Scaling.Replicas,
			CPU:      workload.Scaling.CPU,
			Memory:   workload.Scaling.Memory,
		},
		MetricProfile: workload.MetricProfileRef,
	}
	if deployment.Spec.Replicas != nil {
		report.Replicas = *deployment.Spec.Replicas
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		report.Containers = append(report.Containers, containerReport(container.Name, container.Resources.Requests, container.Resources.Limits))
	}

	hpas, err := a.kube.ListHPAs(ctx, profile.Spec.Namespace)
	if err != nil {
		return WorkloadReport{}, fmt.Errorf("list hpa %s: %w", profile.Spec.Namespace, err)
	}
	for _, hpa := range hpas.Items {
		ref := hpa.Spec.ScaleTargetRef
		if ref.Kind == workload.TargetRef.Kind && ref.Name == workload.TargetRef.Name {
			apiVersion := hpa.APIVersion
			if apiVersion == "" {
				apiVersion = "autoscaling/v2"
			}
			report.Autoscalers = append(report.Autoscalers, Autoscaler{
				Kind:       "HorizontalPodAutoscaler",
				Name:       hpa.Name,
				APIVersion: apiVersion,
			})
		}
	}
	if len(report.Autoscalers) > 0 && (workload.Scaling.Replicas || workload.Scaling.CPU || workload.Scaling.Memory) {
		report.CommitBlocked = true
		report.BlockReasons = append(report.BlockReasons, "autoscaler targets workload; remove HPA/VPA/KEDA from Git before commit mode")
	}
	if !report.FleetManaged {
		report.CommitBlocked = true
		report.BlockReasons = append(report.BlockReasons, "deployment is not Fleet-managed")
	}

	pdbs, err := a.kube.ListPDBs(ctx, profile.Spec.Namespace)
	if err != nil {
		return WorkloadReport{}, fmt.Errorf("list pdb %s: %w", profile.Spec.Namespace, err)
	}
	for _, pdb := range pdbs.Items {
		if pdbMatchesDeployment(pdb, deployment.Spec.Template.Labels) {
			report.PDBs = append(report.PDBs, pdbReport(pdb))
		}
	}
	availability, err := a.availabilityReport(ctx, profile.Spec.Namespace, deployment)
	if err != nil {
		return WorkloadReport{}, fmt.Errorf("build availability report %s/%s: %w", profile.Spec.Namespace, workload.TargetRef.Name, err)
	}
	report.Availability = availability
	rollout, err := a.rolloutReport(ctx, profile.Spec.Namespace, deployment)
	if err != nil {
		return WorkloadReport{}, fmt.Errorf("build rollout report %s/%s: %w", profile.Spec.Namespace, workload.TargetRef.Name, err)
	}
	report.Rollout = rollout

	metricProfile := profile.MetricProfiles[workload.MetricProfileRef]
	vars := workload.VarsWithDefaults(profile.Spec.Namespace)
	for signalName, signal := range metricProfile.Signals {
		report.MetricSignals = append(report.MetricSignals, a.validateSignal(ctx, signalName, signal, vars))
	}
	report.MetricsCondition = metricCondition(report.MetricSignals)
	report.Recommendation = buildRecommendation(workload, report, sharedSignals)

	return report, nil
}

func (a *Analyzer) rolloutReport(ctx context.Context, namespace string, deployment *appsv1.Deployment) (RolloutReport, error) {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	report := RolloutReport{
		Evaluated:           true,
		Settled:             true,
		Generation:          deployment.Generation,
		ObservedGeneration:  deployment.Status.ObservedGeneration,
		DesiredReplicas:     desired,
		UpdatedReplicas:     deployment.Status.UpdatedReplicas,
		ReadyReplicas:       deployment.Status.ReadyReplicas,
		AvailableReplicas:   deployment.Status.AvailableReplicas,
		UnavailableReplicas: deployment.Status.UnavailableReplicas,
	}
	if report.ObservedGeneration != report.Generation {
		report.Reasons = append(report.Reasons, fmt.Sprintf("observed_generation_pending:%d/%d", report.ObservedGeneration, report.Generation))
	}
	if report.UpdatedReplicas < desired {
		report.Reasons = append(report.Reasons, fmt.Sprintf("updated_replicas_pending:%d/%d", report.UpdatedReplicas, desired))
	}
	if report.ReadyReplicas < desired {
		report.Reasons = append(report.Reasons, fmt.Sprintf("ready_replicas_pending:%d/%d", report.ReadyReplicas, desired))
	}
	if report.AvailableReplicas < desired {
		report.Reasons = append(report.Reasons, fmt.Sprintf("available_replicas_pending:%d/%d", report.AvailableReplicas, desired))
	}
	if report.UnavailableReplicas > 0 {
		report.Reasons = append(report.Reasons, fmt.Sprintf("unavailable_replicas:%d", report.UnavailableReplicas))
	}

	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return report, err
	}
	pods, err := a.kube.ListPods(ctx, namespace, selector.String())
	if err != nil {
		return report, err
	}
	for _, pod := range pods.Items {
		switch {
		case pod.DeletionTimestamp != nil:
			report.TerminatingPods++
		case pod.Status.Phase == corev1.PodPending:
			report.PendingPods++
		}
		if !podReady(pod) && pod.DeletionTimestamp == nil {
			report.UnreadyPods++
		}
		if podHasIncompleteInit(pod) {
			report.IncompleteInitPods++
		}
	}
	if report.TerminatingPods > 0 {
		report.Reasons = append(report.Reasons, fmt.Sprintf("terminating_pods:%d", report.TerminatingPods))
	}
	if report.PendingPods > 0 {
		report.Reasons = append(report.Reasons, fmt.Sprintf("pending_pods:%d", report.PendingPods))
	}
	if report.IncompleteInitPods > 0 {
		report.Reasons = append(report.Reasons, fmt.Sprintf("incomplete_init_pods:%d", report.IncompleteInitPods))
	}
	if report.UnreadyPods > 0 {
		report.Reasons = append(report.Reasons, fmt.Sprintf("unready_pods:%d", report.UnreadyPods))
	}
	report.Settled = len(report.Reasons) == 0
	return report, nil
}

func (a *Analyzer) availabilityReport(ctx context.Context, namespace string, deployment *appsv1.Deployment) (AvailabilityReport, error) {
	report := AvailabilityReport{
		ReadyEndpoints: deployment.Status.ReadyReplicas,
	}
	services, err := a.kube.ListServices(ctx, namespace)
	if err != nil {
		return report, err
	}
	for _, service := range services.Items {
		if serviceMatchesDeployment(service, deployment.Spec.Template.Labels) {
			report.Services = append(report.Services, service.Name)
			if service.Spec.Type == corev1.ServiceTypeLoadBalancer || service.Spec.Type == corev1.ServiceTypeNodePort {
				report.Public = true
			}
		}
	}
	sort.Strings(report.Services)
	if deployment.Annotations["field.cattle.io/publicEndpoints"] != "" {
		report.Public = true
	}
	report.RollingUpdateZeroUnavailable = rollingUpdateZeroUnavailable(deployment)

	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return report, err
	}
	pods, err := a.kube.ListPods(ctx, namespace, selector.String())
	if err != nil {
		return report, err
	}
	readyNodes := map[string]struct{}{}
	for _, pod := range pods.Items {
		if podReady(pod) && pod.Spec.NodeName != "" {
			readyNodes[pod.Spec.NodeName] = struct{}{}
		}
	}
	report.ReadyNodes = len(readyNodes)

	if report.Public && report.ReadyEndpoints >= 2 {
		report.ReplicaFloor = 2
		report.Reasons = append(report.Reasons, "public_service")
		if len(report.Services) > 0 {
			report.Reasons = append(report.Reasons, "service_endpoints")
		}
		if report.ReadyNodes >= 2 {
			report.Reasons = append(report.Reasons, "multi_node_ready_endpoints")
		}
		if report.RollingUpdateZeroUnavailable {
			report.Reasons = append(report.Reasons, "zero_unavailable_rollout")
		}
	}
	return report, nil
}

func serviceMatchesDeployment(service corev1.Service, podLabels map[string]string) bool {
	if len(service.Spec.Selector) == 0 {
		return false
	}
	for key, value := range service.Spec.Selector {
		if podLabels[key] != value {
			return false
		}
	}
	return true
}

func rollingUpdateZeroUnavailable(deployment *appsv1.Deployment) bool {
	if deployment.Spec.Strategy.Type != "" && deployment.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		return false
	}
	if deployment.Spec.Strategy.RollingUpdate == nil || deployment.Spec.Strategy.RollingUpdate.MaxUnavailable == nil {
		return false
	}
	return intstrCeil(*deployment.Spec.Strategy.RollingUpdate.MaxUnavailable, maxInt32(derefInt32(deployment.Spec.Replicas), 1)) == 0
}

func podReady(pod corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podHasIncompleteInit(pod corev1.Pod) bool {
	if len(pod.Spec.InitContainers) == 0 {
		return false
	}
	completed := map[string]bool{}
	for _, status := range pod.Status.InitContainerStatuses {
		completed[status.Name] = status.Ready || (status.State.Terminated != nil && status.State.Terminated.ExitCode == 0)
	}
	for _, container := range pod.Spec.InitContainers {
		if !completed[container.Name] {
			return true
		}
	}
	return false
}

func derefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func pdbMatchesDeployment(pdb policyv1.PodDisruptionBudget, podLabels map[string]string) bool {
	if pdb.Spec.Selector == nil {
		return false
	}
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(podLabels))
}

func pdbReport(pdb policyv1.PodDisruptionBudget) PDBReport {
	report := PDBReport{
		Name:               pdb.Name,
		DisruptionsAllowed: pdb.Status.DisruptionsAllowed,
		DesiredHealthy:     pdb.Status.DesiredHealthy,
		CurrentHealthy:     pdb.Status.CurrentHealthy,
		ExpectedPods:       pdb.Status.ExpectedPods,
	}
	if pdb.Spec.MinAvailable != nil {
		report.MinAvailable = pdb.Spec.MinAvailable.String()
		report.MinimumReplicaFloor = intstrCeil(*pdb.Spec.MinAvailable, maxInt32(pdb.Status.ExpectedPods, 1))
		report.ScaleDownFloorEnforced = report.MinimumReplicaFloor > 0
	}
	if pdb.Spec.MaxUnavailable != nil {
		report.MaxUnavailable = pdb.Spec.MaxUnavailable.String()
	}
	return report
}

func intstrCeil(value intstr.IntOrString, total int32) int32 {
	switch value.Type {
	case intstr.Int:
		return int32(value.IntValue())
	case intstr.String:
		parsed, err := intstr.GetScaledValueFromIntOrPercent(&value, int(total), true)
		if err != nil {
			return 0
		}
		return int32(parsed)
	default:
		return 0
	}
}

func (a *Analyzer) validateSignal(ctx context.Context, name string, signal config.Signal, vars map[string]string) SignalReport {
	rendered, err := config.RenderQuery(signal.Query, vars)
	if err != nil {
		return SignalReport{
			Name:     name,
			Required: signal.Required,
			Query:    signal.Query,
			Error:    err.Error(),
		}
	}
	report := SignalReport{
		Name:     name,
		Required: signal.Required,
		Query:    rendered,
	}
	result, err := a.prom.Query(ctx, rendered)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	report.ResultType = result.ResultType
	report.Series = len(result.Series)
	if len(result.Series) > 0 {
		value := result.Series[0].Value
		report.Sample = &value
		report.Healthy = true
	}
	end := time.Now().UTC()
	start := end.Add(-a.options.HistoryWindow)
	rangeResult, err := a.prom.QueryRange(ctx, rendered, start, end, a.options.HistoryStep)
	if err != nil {
		report.HistoryError = err.Error()
		return report
	}
	samples := aggregateRangeSamples(rangeResult)
	history := summarizeSamples(samples, a.options.HistoryWindow, a.options.HistoryStep)
	if history != nil {
		report.History = history
		if forecastSignalEnabled(name) {
			report.Forecast = buildSignalForecast(samples, report.Sample, *history)
			a.attachForecastBaselines(ctx, rendered, end, &report)
		}
	}
	report.Anomaly = classifyAnomaly(report)
	return report
}

func forecastSignalEnabled(name string) bool {
	switch name {
	case "available_replicas":
		return false
	default:
		return true
	}
}

func (a *Analyzer) attachForecastBaselines(ctx context.Context, query string, end time.Time, report *SignalReport) {
	if report.Forecast == nil {
		report.Forecast = &SignalForecast{}
	}
	baselines := []struct {
		name   string
		offset time.Duration
	}{
		{name: "same_time_yesterday", offset: 24 * time.Hour},
		{name: "same_weekday", offset: 7 * 24 * time.Hour},
	}
	for _, baseline := range baselines {
		baselineEnd := end.Add(-baseline.offset)
		baselineStart := baselineEnd.Add(-a.options.HistoryWindow)
		result, err := a.prom.QueryRange(ctx, query, baselineStart, baselineEnd, a.options.HistoryStep)
		if err != nil {
			continue
		}
		summary := summarizeSamples(aggregateRangeSamples(result), a.options.HistoryWindow, a.options.HistoryStep)
		if summary == nil {
			continue
		}
		report.Forecast.Baselines = append(report.Forecast.Baselines, ForecastBaseline{
			Name:   baseline.name,
			Window: summary.Window,
			Points: summary.Points,
			P50:    summary.P50,
			P95:    summary.P95,
			Max:    summary.Max,
		})
	}
	if len(report.Forecast.Horizons) == 0 && len(report.Forecast.Baselines) == 0 {
		report.Forecast = nil
	}
}

func containerReport(name string, requests, limits corev1.ResourceList) ContainerReport {
	report := ContainerReport{Name: name}
	if value, ok := requests[corev1.ResourceCPU]; ok {
		report.CPURequest = value.String()
		report.CPURequestCores = float64(value.MilliValue()) / 1000
	}
	if value, ok := requests[corev1.ResourceMemory]; ok {
		report.MemoryRequest = value.String()
		report.MemoryRequestBytes = float64(value.Value())
	}
	if value, ok := limits[corev1.ResourceCPU]; ok {
		report.CPULimit = value.String()
	}
	if value, ok := limits[corev1.ResourceMemory]; ok {
		report.MemoryLimit = value.String()
	}
	return report
}

func buildRecommendation(workload config.WorkloadSpec, report WorkloadReport, sharedSignals []SignalReport) Recommendation {
	recommendation := Recommendation{
		Mode:                "dry-run",
		CurrentReplicas:     report.Replicas,
		RecommendedReplicas: report.Replicas,
		Confidence:          0.5,
		Learning: LearningEvidence{
			Mode:        "prometheus-history",
			Description: "learned from Prometheus range history for this analysis window; persistent model state is not enabled yet",
		},
		Blocked:      report.CommitBlocked,
		BlockReasons: append([]string(nil), report.BlockReasons...),
	}

	if len(report.Containers) != 1 {
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, "multi_container_recommendation_not_implemented")
		return recommendation
	}

	container := report.Containers[0]
	recommendation.CurrentCPURequest = container.CPURequest
	recommendation.RecommendedCPURequest = container.CPURequest
	recommendation.CurrentMemoryRequest = container.MemoryRequest
	recommendation.RecommendedMemoryRequest = container.MemoryRequest

	replicas := float64(maxInt32(report.Replicas, 1))
	cpuUsage, hasCPU := signalSample(report.MetricSignals, "cpu_usage")
	memoryUsage, hasMemory := signalSample(report.MetricSignals, "memory_working_set")
	requestRate, hasRequestRate := signalSample(report.MetricSignals, "request_rate")
	latencyP95, hasLatency := signalSample(report.MetricSignals, "latency_p95")
	errorRate, hasErrorRate := signalSample(report.MetricSignals, "error_rate")
	if anomalies := activeAnomalies(report.MetricSignals); len(anomalies) > 0 {
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, anomalies...)
	}
	if anomalies := activeAnomalies(sharedSignals); len(anomalies) > 0 {
		for _, anomaly := range anomalies {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "shared_"+anomaly)
		}
	}

	if hasRequestRate {
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("request_rate_observed:%.4g", requestRate))
		if history := signalHistory(report.MetricSignals, "request_rate"); history != nil {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("request_rate_p95_%s:%.4g", history.Window, history.P95))
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("request_rate_max_%s:%.4g", history.Window, history.Max))
		}
	}
	if hasLatency {
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("latency_p95_observed:%.4gs", latencyP95))
	}
	if hasErrorRate {
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("error_rate_observed:%.4g", errorRate))
	}

	cpuPolicy := learnedResourcePolicy(report.MetricSignals, "cpu_usage", cpuPolicyProfile(), workload.Bounds.CPU)
	memoryPolicy := learnedResourcePolicy(report.MetricSignals, "memory_working_set", memoryPolicyProfile(), workload.Bounds.Memory)

	cpuDrivenReplicas := int32(0)
	cpuForDecision := 0.0
	hasCPUDecision := false
	if hasCPU && container.CPURequestCores > 0 {
		cpuForDecision = cpuUsage
		if history := signalHistory(report.MetricSignals, "cpu_usage"); history != nil {
			cpuForDecision = math.Max(cpuForDecision, history.P95)
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("cpu_usage_p95_%s:%.4g", history.Window, history.P95))
		}
		hasCPUDecision = true
		cpuUtilization := cpuForDecision / (container.CPURequestCores * replicas)
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("cpu_utilization:%.1f%%", cpuUtilization*100))
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, learnedPolicyReason("cpu_policy_learned", cpuPolicy))
		cpuDrivenReplicas = int32(math.Ceil(cpuForDecision / (container.CPURequestCores * cpuPolicy.TargetUtilization)))
		if cpuDrivenReplicas > 0 {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("cpu_replicas:%d", cpuDrivenReplicas))
		}
		if workload.Scaling.CPU && cpuUtilization > cpuPolicy.IncreaseThreshold {
			perPodCPU := cpuForDecision / replicas
			recommendedCPU := perPodCPU / cpuPolicy.TargetUtilization
			recommendation.RecommendedCPURequest = boundedCPURequest(container.CPURequestCores, recommendedCPU, cpuPolicy.MaxIncreasePercent, cpuPolicy.MaxDecreasePercent)
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "cpu_request_increase_recommended")
		} else if workload.Scaling.CPU && cpuUtilization < cpuPolicy.DecreaseThreshold && resourceDownscaleAllowed(report.MetricSignals, "cpu_usage") {
			perPodCPU := cpuForDecision / replicas
			recommendedCPU := perPodCPU / cpuPolicy.TargetUtilization
			recommendation.RecommendedCPURequest = boundedCPURequest(container.CPURequestCores, recommendedCPU, cpuPolicy.MaxIncreasePercent, cpuPolicy.MaxDecreasePercent)
			if recommendation.RecommendedCPURequest != container.CPURequest {
				recommendation.ReasonCodes = append(recommendation.ReasonCodes, "cpu_request_decrease_recommended")
			} else {
				recommendation.ReasonCodes = append(recommendation.ReasonCodes, "cpu_request_hold")
			}
		} else {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "cpu_request_hold")
		}
	}

	memoryDrivenReplicas := int32(0)
	memoryForDecision := 0.0
	hasMemoryDecision := false
	if hasMemory && container.MemoryRequestBytes > 0 {
		memoryForDecision = memoryUsage
		if history := signalHistory(report.MetricSignals, "memory_working_set"); history != nil {
			memoryForDecision = math.Max(memoryForDecision, history.P95)
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("memory_working_set_p95_%s:%s", history.Window, formatBytes(history.P95)))
		}
		hasMemoryDecision = true
		memoryUtilization := memoryForDecision / (container.MemoryRequestBytes * replicas)
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("memory_utilization:%.1f%%", memoryUtilization*100))
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, learnedPolicyReason("memory_policy_learned", memoryPolicy))
		memoryDrivenReplicas = int32(math.Ceil(memoryForDecision / (container.MemoryRequestBytes * memoryPolicy.TargetUtilization)))
		if memoryDrivenReplicas > 0 {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("memory_replicas:%d", memoryDrivenReplicas))
		}
		if workload.Scaling.Memory && memoryUtilization > memoryPolicy.IncreaseThreshold {
			perPodMemory := memoryForDecision / replicas
			recommendedMemory := perPodMemory / memoryPolicy.TargetUtilization
			recommendation.RecommendedMemoryRequest = boundedMemoryRequest(container.MemoryRequestBytes, recommendedMemory, memoryPolicy.MaxIncreasePercent, memoryPolicy.MaxDecreasePercent)
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "memory_request_increase_recommended")
		} else if workload.Scaling.Memory && memoryUtilization < memoryPolicy.DecreaseThreshold && resourceDownscaleAllowed(report.MetricSignals, "memory_working_set") {
			perPodMemory := memoryForDecision / replicas
			recommendedMemory := perPodMemory / memoryPolicy.TargetUtilization
			recommendation.RecommendedMemoryRequest = boundedMemoryRequest(container.MemoryRequestBytes, recommendedMemory, memoryPolicy.MaxIncreasePercent, memoryPolicy.MaxDecreasePercent)
			if recommendation.RecommendedMemoryRequest != container.MemoryRequest {
				recommendation.ReasonCodes = append(recommendation.ReasonCodes, "memory_request_decrease_recommended")
			} else {
				recommendation.ReasonCodes = append(recommendation.ReasonCodes, "memory_request_hold")
			}
		} else {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "memory_request_hold")
		}
	}

	trafficDecision := trafficReplicaDecision(report, sharedSignals, replicas)
	if trafficDecision.Replicas > 0 {
		recommendation.ReasonCodes = append(recommendation.ReasonCodes,
			fmt.Sprintf("traffic_forecast:%.4g", trafficDecision.Forecast),
			"traffic_forecast_basis:"+trafficDecision.ForecastBasis,
			fmt.Sprintf("traffic_learned_peak_per_replica:%.4g", trafficDecision.LearnedPeakPerReplica),
			fmt.Sprintf("traffic_pressure_multiplier:%.3g", trafficDecision.PressureMultiplier),
			fmt.Sprintf("traffic_scale_up_allowed:%t", trafficDecision.ScaleUpAllowed),
			fmt.Sprintf("traffic_replicas:%d", trafficDecision.Replicas),
		)
	}

	if workload.Scaling.Replicas {
		pdbFloor := pdbReplicaFloor(report.PDBs)
		if pdbFloor > 0 {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("pdb_replica_floor:%d", pdbFloor))
		}
		availabilityFloor := report.Availability.ReplicaFloor
		if availabilityFloor > 0 {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("availability_replica_floor:%d", availabilityFloor))
			if len(report.Availability.Reasons) > 0 {
				recommendation.ReasonCodes = append(recommendation.ReasonCodes, "availability_floor_reasons:"+strings.Join(report.Availability.Reasons, ","))
			}
		}
		trafficFloor := int32(0)
		if trafficDecision.ScaleUpAllowed {
			trafficFloor = trafficDecision.Replicas
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("traffic_replica_floor:%d", trafficFloor))
		} else if trafficDecision.Replicas > 0 {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("traffic_hold_reference:%d", trafficDecision.Replicas))
		}
		replicaDecision := multiSignalReplicaDecision(workload, report, sharedSignals, trafficDecision, trafficFloor, cpuDrivenReplicas, memoryDrivenReplicas, cpuPolicy, memoryPolicy, hasCPUDecision, cpuForDecision, hasMemoryDecision, memoryForDecision, pdbFloor, availabilityFloor)
		recommendation.ReplicaDecision = &replicaDecision
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, replicaDecisionReason(replicaDecision)...)
		rawReplicas := replicaDecision.RecommendedReplicas
		if workload.Bounds.Replicas.Max > 0 && rawReplicas > int32(workload.Bounds.Replicas.Max) {
			rawReplicas = int32(workload.Bounds.Replicas.Max)
			recommendation.ReplicaDecision.RecommendedReplicas = rawReplicas
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "replica_recommendation_clamped_to_max")
		}
		capacityCeiling := maxInt32(rawReplicas, trafficDecision.Replicas, cpuDrivenReplicas, memoryDrivenReplicas, report.Replicas)
		if plan, ok := optimizeReplicaResourcePlan(workload, report, container, rawReplicas, capacityCeiling, cpuPolicy, memoryPolicy, hasCPUDecision, cpuForDecision, resourceDownscaleAllowed(report.MetricSignals, "cpu_usage"), hasMemoryDecision, memoryForDecision, resourceDownscaleAllowed(report.MetricSignals, "memory_working_set")); ok {
			rawReplicas = plan.Replicas
			if workload.Scaling.CPU && plan.CPURequest != "" {
				recommendation.RecommendedCPURequest = plan.CPURequest
			}
			if workload.Scaling.Memory && plan.MemoryRequest != "" {
				recommendation.RecommendedMemoryRequest = plan.MemoryRequest
			}
			recommendation.ReasonCodes = append(recommendation.ReasonCodes,
				fmt.Sprintf("replica_joint_optimizer_selected:replicas=%d,total_cpu=%.4g,total_memory=%s", plan.Replicas, plan.TotalCPUCores, formatBytes(plan.TotalMemoryBytes)),
			)
			replaceJointResourceReason(&recommendation, "cpu", recommendation.CurrentCPURequest, recommendation.RecommendedCPURequest)
			replaceJointResourceReason(&recommendation, "memory", recommendation.CurrentMemoryRequest, recommendation.RecommendedMemoryRequest)
		}
		if rawReplicas < report.Replicas {
			if reason := replicaDownscaleBlockReason(report, sharedSignals); reason != "" {
				rawReplicas = report.Replicas
				recommendation.ReasonCodes = append(recommendation.ReasonCodes, reason)
			} else {
				recommendation.ReasonCodes = append(recommendation.ReasonCodes, "replica_scale_down_recommended")
			}
		}
		recommendation.RecommendedReplicas = rawReplicas
		if rawReplicas > report.Replicas {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "replica_scale_up_recommended")
		} else if rawReplicas == report.Replicas {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "replica_count_hold")
		}
	} else {
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, "replica_management_disabled")
	}

	applyMinimumResourceChangeThreshold(workload, &recommendation)
	recommendation.Waste = buildWasteScore(recommendation)
	if recommendation.Waste.Summary != "" {
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, "waste_score:"+recommendation.Waste.Summary)
	}
	recommendation.Learning = buildLearningEvidence(workload, report, recommendation, trafficDecision, cpuDrivenReplicas, memoryDrivenReplicas)
	baseConfidence := recommendationConfidence(report, hasCPU, hasMemory, hasRequestRate)
	applyConfidenceDecay(workload, report, sharedSignals, &recommendation, baseConfidence)
	return recommendation
}

func applyMinimumResourceChangeThreshold(workload config.WorkloadSpec, recommendation *Recommendation) {
	if workload.Scaling.CPU {
		if below, change := resourceChangeBelowThreshold(recommendation.CurrentCPURequest, recommendation.RecommendedCPURequest, workload.Bounds.CPU.MinChangePercent); below {
			recommendation.RecommendedCPURequest = recommendation.CurrentCPURequest
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("cpu_request_change_below_min_percent:%.2f<%.2f", change, workload.Bounds.CPU.MinChangePercent))
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "cpu_request_hold_min_change_threshold")
		}
	}
	if workload.Scaling.Memory {
		if below, change := resourceChangeBelowThreshold(recommendation.CurrentMemoryRequest, recommendation.RecommendedMemoryRequest, workload.Bounds.Memory.MinChangePercent); below {
			recommendation.RecommendedMemoryRequest = recommendation.CurrentMemoryRequest
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, fmt.Sprintf("memory_request_change_below_min_percent:%.2f<%.2f", change, workload.Bounds.Memory.MinChangePercent))
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "memory_request_hold_min_change_threshold")
		}
	}
}

const monthlyEstimateHours = 730.0

func buildWasteScore(recommendation Recommendation) WasteScore {
	currentReplicas := float64(maxInt32(recommendation.CurrentReplicas, 0))
	recommendedReplicas := float64(maxInt32(recommendation.RecommendedReplicas, 0))
	currentCPU, currentCPUOK := cpuRequestCores(recommendation.CurrentCPURequest)
	recommendedCPU, recommendedCPUOK := cpuRequestCores(recommendation.RecommendedCPURequest)
	currentMemory, currentMemoryOK := memoryRequestBytes(recommendation.CurrentMemoryRequest)
	recommendedMemory, recommendedMemoryOK := memoryRequestBytes(recommendation.RecommendedMemoryRequest)

	score := WasteScore{MonthlyHours: monthlyEstimateHours}
	score.CurrentHourly.ReplicaHours = currentReplicas
	score.RecommendedHourly.ReplicaHours = recommendedReplicas
	score.HourlyReduction.ReplicaHours = currentReplicas - recommendedReplicas
	if currentCPUOK && recommendedCPUOK {
		score.CurrentHourly.CPUCoreHours = currentCPU * currentReplicas
		score.RecommendedHourly.CPUCoreHours = recommendedCPU * recommendedReplicas
		score.HourlyReduction.CPUCoreHours = score.CurrentHourly.CPUCoreHours - score.RecommendedHourly.CPUCoreHours
	}
	if currentMemoryOK && recommendedMemoryOK {
		score.CurrentHourly.MemoryGiBHours = bytesToGiB(currentMemory) * currentReplicas
		score.RecommendedHourly.MemoryGiBHours = bytesToGiB(recommendedMemory) * recommendedReplicas
		score.HourlyReduction.MemoryGiBHours = score.CurrentHourly.MemoryGiBHours - score.RecommendedHourly.MemoryGiBHours
	}
	score.MonthlyReduction = ResourceHours{
		CPUCoreHours:   score.HourlyReduction.CPUCoreHours * monthlyEstimateHours,
		MemoryGiBHours: score.HourlyReduction.MemoryGiBHours * monthlyEstimateHours,
		ReplicaHours:   score.HourlyReduction.ReplicaHours * monthlyEstimateHours,
	}
	score.Summary = wasteSummary(score)
	return score
}

func wasteSummary(score WasteScore) string {
	if score.HourlyReduction.CPUCoreHours == 0 && score.HourlyReduction.MemoryGiBHours == 0 && score.HourlyReduction.ReplicaHours == 0 {
		return "no_resource_hour_change"
	}
	direction := "reduction"
	if score.HourlyReduction.CPUCoreHours < 0 || score.HourlyReduction.MemoryGiBHours < 0 || score.HourlyReduction.ReplicaHours < 0 {
		direction = "capacity_increase"
	}
	return fmt.Sprintf("%s monthly_cpu=%+.1f_core_hours monthly_memory=%+.1f_gib_hours monthly_replicas=%+.0f_replica_hours", direction, score.MonthlyReduction.CPUCoreHours, score.MonthlyReduction.MemoryGiBHours, score.MonthlyReduction.ReplicaHours)
}

func bytesToGiB(value float64) float64 {
	return value / (1024 * 1024 * 1024)
}

func resourceChangeBelowThreshold(current, recommended string, thresholdPercent float64) (bool, float64) {
	if thresholdPercent <= 0 || current == "" || recommended == "" || current == recommended {
		return false, 0
	}
	currentQuantity, err := resource.ParseQuantity(current)
	if err != nil {
		return false, 0
	}
	recommendedQuantity, err := resource.ParseQuantity(recommended)
	if err != nil {
		return false, 0
	}
	currentValue := math.Abs(currentQuantity.AsApproximateFloat64())
	if currentValue <= 0 {
		return false, 0
	}
	changePercent := math.Abs(recommendedQuantity.AsApproximateFloat64()-currentQuantity.AsApproximateFloat64()) / currentValue * 100
	return changePercent < thresholdPercent, changePercent
}

func replicaFloorForResource(managed bool, replicas int32) int32 {
	if managed {
		return 0
	}
	return replicas
}

type replicaResourcePlan struct {
	Replicas         int32
	CPURequest       string
	MemoryRequest    string
	TotalCPUCores    float64
	TotalMemoryBytes float64
	Score            float64
}

func optimizeReplicaResourcePlan(workload config.WorkloadSpec, report WorkloadReport, container ContainerReport, floor, ceiling int32, cpuPolicy, memoryPolicy resourcePolicy, hasCPU bool, cpuDemand float64, canDecreaseCPU bool, hasMemory bool, memoryDemand float64, canDecreaseMemory bool) (replicaResourcePlan, bool) {
	if floor < 1 {
		floor = 1
	}
	ceiling = maxInt32(floor, ceiling, report.Replicas)
	if workload.Bounds.Replicas.Max > 0 && ceiling > int32(workload.Bounds.Replicas.Max) {
		ceiling = int32(workload.Bounds.Replicas.Max)
	}

	currentTotalCPU := container.CPURequestCores * float64(maxInt32(report.Replicas, 1))
	currentTotalMemory := container.MemoryRequestBytes * float64(maxInt32(report.Replicas, 1))
	var best replicaResourcePlan
	for replicas := floor; replicas <= ceiling; replicas++ {
		plan, ok := candidateReplicaResourcePlan(workload, container, replicas, cpuPolicy, memoryPolicy, hasCPU, cpuDemand, canDecreaseCPU, hasMemory, memoryDemand, canDecreaseMemory, currentTotalCPU, currentTotalMemory)
		if !ok {
			continue
		}
		if best.Replicas == 0 || plan.Score < best.Score {
			best = plan
		}
	}
	return best, best.Replicas > 0
}

func candidateReplicaResourcePlan(workload config.WorkloadSpec, container ContainerReport, replicas int32, cpuPolicy, memoryPolicy resourcePolicy, hasCPU bool, cpuDemand float64, canDecreaseCPU bool, hasMemory bool, memoryDemand float64, canDecreaseMemory bool, currentTotalCPU, currentTotalMemory float64) (replicaResourcePlan, bool) {
	plan := replicaResourcePlan{Replicas: replicas}
	replicaCount := float64(maxInt32(replicas, 1))

	cpuCores := container.CPURequestCores
	plan.CPURequest = container.CPURequest
	if hasCPU && cpuDemand > 0 && cpuPolicy.TargetUtilization > 0 && container.CPURequestCores > 0 {
		targetCPU := cpuDemand / (replicaCount * cpuPolicy.TargetUtilization)
		if workload.Scaling.CPU {
			if targetCPU < container.CPURequestCores && !canDecreaseCPU {
				plan.CPURequest = container.CPURequest
			} else {
				plan.CPURequest = boundedCPURequest(container.CPURequestCores, targetCPU, cpuPolicy.MaxIncreasePercent, cpuPolicy.MaxDecreasePercent)
				parsedCPU, ok := cpuRequestCores(plan.CPURequest)
				if !ok || parsedCPU*replicaCount*cpuPolicy.TargetUtilization < cpuDemand*0.99 {
					return replicaResourcePlan{}, false
				}
				cpuCores = parsedCPU
			}
		} else if container.CPURequestCores*replicaCount*cpuPolicy.TargetUtilization < cpuDemand*0.99 {
			return replicaResourcePlan{}, false
		}
	}

	memoryBytes := container.MemoryRequestBytes
	plan.MemoryRequest = container.MemoryRequest
	if hasMemory && memoryDemand > 0 && memoryPolicy.TargetUtilization > 0 && container.MemoryRequestBytes > 0 {
		targetMemory := memoryDemand / (replicaCount * memoryPolicy.TargetUtilization)
		if workload.Scaling.Memory {
			if targetMemory < container.MemoryRequestBytes && !canDecreaseMemory {
				plan.MemoryRequest = container.MemoryRequest
			} else {
				plan.MemoryRequest = boundedMemoryRequest(container.MemoryRequestBytes, targetMemory, memoryPolicy.MaxIncreasePercent, memoryPolicy.MaxDecreasePercent)
				parsedMemory, ok := memoryRequestBytes(plan.MemoryRequest)
				if !ok || parsedMemory*replicaCount*memoryPolicy.TargetUtilization < memoryDemand*0.99 {
					return replicaResourcePlan{}, false
				}
				memoryBytes = parsedMemory
			}
		} else if container.MemoryRequestBytes*replicaCount*memoryPolicy.TargetUtilization < memoryDemand*0.99 {
			return replicaResourcePlan{}, false
		}
	}

	plan.TotalCPUCores = cpuCores * replicaCount
	plan.TotalMemoryBytes = memoryBytes * replicaCount
	cpuScore := normalizedResourceScore(plan.TotalCPUCores, currentTotalCPU)
	memoryScore := normalizedResourceScore(plan.TotalMemoryBytes, currentTotalMemory)
	plan.Score = cpuScore + memoryScore + (float64(replicas) * 0.02)
	return plan, true
}

func normalizedResourceScore(value, baseline float64) float64 {
	if baseline <= 0 {
		return 0
	}
	return value / baseline
}

func cpuRequestCores(value string) (float64, bool) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, false
	}
	return float64(quantity.MilliValue()) / 1000, true
}

func memoryRequestBytes(value string) (float64, bool) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, false
	}
	return float64(quantity.Value()), true
}

func replaceJointResourceReason(recommendation *Recommendation, resourceName, current, recommended string) {
	recommendation.ReasonCodes = withoutResourceRequestReasons(recommendation.ReasonCodes, resourceName)
	if current == "" || recommended == "" || current == recommended {
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, resourceName+"_request_hold")
		return
	}
	currentValue, currentOK := comparableResourceValue(resourceName, current)
	recommendedValue, recommendedOK := comparableResourceValue(resourceName, recommended)
	if !currentOK || !recommendedOK {
		return
	}
	switch {
	case recommendedValue > currentValue:
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, resourceName+"_request_increase_recommended_by_joint_optimizer")
	case recommendedValue < currentValue:
		recommendation.ReasonCodes = append(recommendation.ReasonCodes, resourceName+"_request_decrease_recommended_by_joint_optimizer")
	}
}

func withoutResourceRequestReasons(reasons []string, resourceName string) []string {
	filtered := reasons[:0]
	for _, reason := range reasons {
		if strings.HasPrefix(reason, resourceName+"_request_increase_recommended") ||
			strings.HasPrefix(reason, resourceName+"_request_decrease_recommended") ||
			strings.HasPrefix(reason, resourceName+"_request_hold") {
			continue
		}
		filtered = append(filtered, reason)
	}
	return filtered
}

func reasonWithPrefix(recommendation Recommendation, prefix string) string {
	for _, reason := range recommendation.ReasonCodes {
		if strings.HasPrefix(reason, prefix) {
			return reason
		}
	}
	return ""
}

func comparableResourceValue(resourceName, value string) (float64, bool) {
	switch resourceName {
	case "cpu":
		return cpuRequestCores(value)
	case "memory":
		return memoryRequestBytes(value)
	default:
		return 0, false
	}
}

func pdbReplicaFloor(pdbs []PDBReport) int32 {
	var floor int32
	for _, pdb := range pdbs {
		if pdb.ScaleDownFloorEnforced && pdb.MinimumReplicaFloor > floor {
			floor = pdb.MinimumReplicaFloor
		}
	}
	return floor
}

func buildLearningEvidence(workload config.WorkloadSpec, report WorkloadReport, recommendation Recommendation, traffic trafficDecision, cpuReplicas, memoryReplicas int32) LearningEvidence {
	evidence := LearningEvidence{
		Mode:        "prometheus-history",
		Description: "learned from Prometheus range history for this analysis window; persistent model state is not enabled yet",
	}
	for _, name := range []string{"request_rate", "latency_p95", "cpu_usage", "memory_working_set", "available_replicas"} {
		if learned, ok := learnedSignal(report.MetricSignals, name); ok {
			evidence.Signals = append(evidence.Signals, learned)
		}
	}
	if recommendation.ReplicaDecision != nil {
		evidence.Decisions = append(evidence.Decisions, LearnedDecision{
			Subject:    "replicas.multi_signal",
			Learned:    fmt.Sprintf("score=%.3g floor=%d components=%s", recommendation.ReplicaDecision.Score, recommendation.ReplicaDecision.Floor, replicaDecisionComponentsObserved(recommendation.ReplicaDecision.Components)),
			Observed:   "basis=" + recommendation.ReplicaDecision.Basis,
			Conclusion: fmt.Sprintf("combined signal score recommends %d replica(s)", recommendation.ReplicaDecision.RecommendedReplicas),
		})
	}
	if workload.Scaling.Replicas && traffic.Replicas > 0 {
		conclusion := fmt.Sprintf("traffic needs %d replica(s)", traffic.Replicas)
		if !traffic.ScaleUpAllowed && traffic.Replicas >= report.Replicas {
			conclusion = fmt.Sprintf("traffic is inside learned envelope; use %d replica(s) as a hold reference, not a scale-up floor", report.Replicas)
			if recommendation.RecommendedReplicas < report.Replicas {
				conclusion = "traffic is inside learned envelope; resource optimizer may choose fewer replicas when learned p95 demand still fits"
			}
		}
		evidence.Decisions = append(evidence.Decisions, LearnedDecision{
			Subject:    "replicas.traffic",
			Learned:    fmt.Sprintf("peak_per_replica=%.4g scale_up_allowed=%t", traffic.LearnedPeakPerReplica, traffic.ScaleUpAllowed),
			Observed:   fmt.Sprintf("forecast=%.4g basis=%s pressure_multiplier=%.3g", traffic.Forecast, traffic.ForecastBasis, traffic.PressureMultiplier),
			Conclusion: conclusion,
		})
	}
	if workload.Scaling.Replicas && cpuReplicas > 0 {
		evidence.Decisions = append(evidence.Decisions, LearnedDecision{
			Subject:    "replicas.cpu",
			Learned:    "replica count from learned p95 CPU usage and target utilization",
			Observed:   fmt.Sprintf("cpu_replicas=%d", cpuReplicas),
			Conclusion: fmt.Sprintf("CPU needs %d replica(s)", cpuReplicas),
		})
	}
	if workload.Scaling.Replicas && memoryReplicas > 0 {
		evidence.Decisions = append(evidence.Decisions, LearnedDecision{
			Subject:    "replicas.memory",
			Learned:    "replica count from learned p95 memory working set and target utilization",
			Observed:   fmt.Sprintf("memory_replicas=%d", memoryReplicas),
			Conclusion: fmt.Sprintf("memory needs %d replica(s)", memoryReplicas),
		})
	}
	if workload.Scaling.Replicas && report.Availability.ReplicaFloor > 0 {
		evidence.Decisions = append(evidence.Decisions, LearnedDecision{
			Subject:    "replicas.availability",
			Learned:    "availability floor inferred from public exposure, ready endpoints, rollout semantics, and node spread",
			Observed:   availabilityObserved(report.Availability),
			Conclusion: fmt.Sprintf("availability requires at least %d replica(s)", report.Availability.ReplicaFloor),
		})
	}
	if workload.Scaling.Replicas {
		if reason := reasonWithPrefix(recommendation, "replica_joint_optimizer_selected:"); reason != "" {
			evidence.Decisions = append(evidence.Decisions, LearnedDecision{
				Subject:    "replicas.resource_optimizer",
				Learned:    "candidate replica counts are compared with the per-pod CPU and memory request needed to cover learned p95 demand",
				Observed:   strings.TrimPrefix(reason, "replica_joint_optimizer_selected:"),
				Conclusion: "choose the lowest safe total requested resource plan after availability, traffic, and guardrail floors",
			})
		}
	}
	if !workload.Scaling.Replicas {
		evidence.Decisions = append(evidence.Decisions, LearnedDecision{
			Subject:    "replicas.management",
			Learned:    "replica ownership is disabled for this workload",
			Observed:   fmt.Sprintf("cpu_replicas=%d memory_replicas=%d", cpuReplicas, memoryReplicas),
			Conclusion: "do not recommend replica changes; use learned CPU and memory signals only for request sizing",
		})
	}
	if workload.Scaling.CPU && recommendation.CurrentCPURequest != "" && recommendation.RecommendedCPURequest != "" {
		evidence.Decisions = append(evidence.Decisions, LearnedDecision{
			Subject:    "resources.cpu_request",
			Learned:    "CPU request from learned p95 CPU usage, learned utilization target, and learned change bounds",
			Observed:   fmt.Sprintf("%s -> %s", recommendation.CurrentCPURequest, recommendation.RecommendedCPURequest),
			Conclusion: resourceChangeConclusion("CPU request", recommendation.CurrentCPURequest, recommendation.RecommendedCPURequest),
		})
	}
	if workload.Scaling.Memory && recommendation.CurrentMemoryRequest != "" && recommendation.RecommendedMemoryRequest != "" {
		evidence.Decisions = append(evidence.Decisions, LearnedDecision{
			Subject:    "resources.memory_request",
			Learned:    "memory request from learned p95 working set, learned utilization target, and learned change bounds",
			Observed:   fmt.Sprintf("%s -> %s", recommendation.CurrentMemoryRequest, recommendation.RecommendedMemoryRequest),
			Conclusion: resourceChangeConclusion("memory request", recommendation.CurrentMemoryRequest, recommendation.RecommendedMemoryRequest),
		})
	}
	return evidence
}

func availabilityObserved(availability AvailabilityReport) string {
	parts := []string{
		fmt.Sprintf("public=%t", availability.Public),
		fmt.Sprintf("ready_endpoints=%d", availability.ReadyEndpoints),
		fmt.Sprintf("ready_nodes=%d", availability.ReadyNodes),
		fmt.Sprintf("zero_unavailable_rollout=%t", availability.RollingUpdateZeroUnavailable),
	}
	if len(availability.Services) > 0 {
		parts = append(parts, "services="+strings.Join(availability.Services, ","))
	}
	if len(availability.Reasons) > 0 {
		parts = append(parts, "reasons="+strings.Join(availability.Reasons, ","))
	}
	return strings.Join(parts, " ")
}

func replicaDecisionComponentsObserved(components []ReplicaDecisionComponent) string {
	if len(components) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(components))
	for _, component := range components {
		parts = append(parts, fmt.Sprintf("%s:score=%.3g,replicas=%d,influence=%s", component.Name, component.Score, component.Replicas, component.Influence))
	}
	return strings.Join(parts, " ")
}

func resourceChangeConclusion(subject, current, recommended string) string {
	if current == recommended {
		return subject + " is inside learned target band; hold"
	}
	return subject + " should move toward learned target within learned bounds"
}

func learnedSignal(signals []SignalReport, name string) (LearnedSignal, bool) {
	for _, signal := range signals {
		if signal.Name != name || signal.History == nil {
			continue
		}
		learned := LearnedSignal{
			Name:           signal.Name,
			Window:         signal.History.Window,
			Step:           signal.History.Step,
			Points:         signal.History.Points,
			P50:            signal.History.P50,
			P95:            signal.History.P95,
			Max:            signal.History.Max,
			Classification: "learned_baseline",
		}
		if signal.Sample != nil {
			learned.Current = *signal.Sample
			if signal.History.P95 > 0 {
				learned.CurrentVsP95 = *signal.Sample / signal.History.P95
			}
			if signal.History.Max > 0 {
				learned.CurrentVsMax = *signal.Sample / signal.History.Max
			}
			learned.Classification = learnedClassification(*signal.Sample, signal.History)
		}
		return learned, true
	}
	return LearnedSignal{}, false
}

func learnedClassification(current float64, history *SignalHistory) string {
	if history == nil {
		return "no_history"
	}
	switch {
	case history.Max > 0 && current > history.Max:
		return "above_learned_max"
	case history.P95 > 0 && current >= history.P95:
		return "near_or_above_learned_p95"
	case history.P50 > 0 && current < history.P50:
		return "below_learned_median"
	default:
		return "inside_learned_envelope"
	}
}

func replicaDownscaleBlockReason(report WorkloadReport, sharedSignals []SignalReport) string {
	if report.ReadyReplicas < report.Replicas {
		return "scale_down_blocked_by_unavailable_replicas"
	}
	if hasActiveAnomaly(report.MetricSignals) || hasActiveAnomaly(sharedSignals) {
		return "scale_down_blocked_by_anomaly"
	}
	if !hasReplicaScaleDownHistory(report.MetricSignals) {
		return "scale_down_requires_history"
	}
	return ""
}

func hasReplicaScaleDownHistory(signals []SignalReport) bool {
	required := []string{"available_replicas", "cpu_usage", "memory_working_set"}
	for _, name := range required {
		history := signalHistory(signals, name)
		if history == nil || history.Points < 12 {
			return false
		}
	}
	if history := signalHistory(signals, "request_rate"); history != nil {
		return history.Points >= 12
	}
	return true
}

func resourceDownscaleAllowed(signals []SignalReport, name string) bool {
	for _, signal := range signals {
		if signal.Name != name {
			continue
		}
		if signal.History == nil || signal.History.Points < 12 {
			return false
		}
		return signal.Anomaly.State == "" || signal.Anomaly.State == "normal"
	}
	return false
}

type trafficDecision struct {
	Replicas              int32
	Forecast              float64
	ForecastBasis         string
	LearnedPeakPerReplica float64
	PressureMultiplier    float64
	ScaleUpAllowed        bool
}

func multiSignalReplicaDecision(workload config.WorkloadSpec, report WorkloadReport, sharedSignals []SignalReport, traffic trafficDecision, trafficFloor, cpuReplicas, memoryReplicas int32, cpuPolicy, memoryPolicy resourcePolicy, hasCPU bool, cpuDemand float64, hasMemory bool, memoryDemand float64, pdbFloor, availabilityFloor int32) ReplicaDecision {
	current := maxInt32(report.Replicas, 1)
	configFloor := int32(workload.Bounds.Replicas.Min)
	floor := maxInt32(configFloor, pdbFloor, availabilityFloor, 1)
	decision := ReplicaDecision{
		RecommendedReplicas: current,
		Floor:               floor,
	}
	if configFloor > 0 {
		decision.FloorReasons = append(decision.FloorReasons, fmt.Sprintf("configured_min:%d", configFloor))
	}
	if pdbFloor > 0 {
		decision.FloorReasons = append(decision.FloorReasons, fmt.Sprintf("pdb:%d", pdbFloor))
	}
	if availabilityFloor > 0 {
		decision.FloorReasons = append(decision.FloorReasons, fmt.Sprintf("availability:%d", availabilityFloor))
	}

	trafficComponent := trafficReplicaComponent(traffic, current)
	if trafficComponent.Name != "" {
		decision.Components = append(decision.Components, trafficComponent)
	}
	if component, ok := anomalyReplicaComponent(report.MetricSignals, "latency_p95", "latency", current); ok {
		decision.Components = append(decision.Components, component)
	}
	if component, ok := anomalyReplicaComponent(report.MetricSignals, "error_rate", "error_rate", current); ok {
		decision.Components = append(decision.Components, component)
	}
	if component, ok := anomalyReplicaComponent(sharedSignals, "concurrent_requests", "concurrent_requests", current); ok {
		decision.Components = append(decision.Components, component)
	}
	if component, ok := resourceReplicaComponent("cpu", cpuReplicas, current, hasCPU, cpuDemand, cpuPolicy.TargetUtilization); ok {
		decision.Components = append(decision.Components, component)
	}
	if component, ok := resourceReplicaComponent("memory", memoryReplicas, current, hasMemory, memoryDemand, memoryPolicy.TargetUtilization); ok {
		decision.Components = append(decision.Components, component)
	}
	if trafficFloor > 0 {
		decision.Components = append(decision.Components, ReplicaDecisionComponent{
			Name:      "traffic_floor",
			Score:     0.45,
			Replicas:  trafficFloor,
			Basis:     "traffic_scale_up_floor",
			Influence: "floor",
		})
	}
	if floor > 1 {
		decision.Components = append(decision.Components, ReplicaDecisionComponent{
			Name:      "availability_floor",
			Score:     0,
			Replicas:  floor,
			Basis:     strings.Join(decision.FloorReasons, "+"),
			Influence: "floor",
		})
	}

	var score float64
	highestPressureReplicas := floor
	lowestResourceReplicas := maxInt32(floor, replicaFloorForResource(workload.Scaling.CPU, cpuReplicas), replicaFloorForResource(workload.Scaling.Memory, memoryReplicas))
	if lowestResourceReplicas == 0 {
		lowestResourceReplicas = floor
	}
	for _, component := range decision.Components {
		score += component.Score
		if component.Score > 0 && component.Replicas > highestPressureReplicas {
			highestPressureReplicas = component.Replicas
		}
	}
	decision.Score = clampFloat(score, -1.5, 1.5)
	switch {
	case decision.Score >= 0.25:
		decision.RecommendedReplicas = maxInt32(current, highestPressureReplicas, floor)
	case decision.Score <= -0.05:
		decision.RecommendedReplicas = maxInt32(lowestResourceReplicas, floor)
	default:
		decision.RecommendedReplicas = maxInt32(current, floor)
	}
	decision.Basis = replicaDecisionBasis(decision)
	return decision
}

func trafficReplicaComponent(traffic trafficDecision, current int32) ReplicaDecisionComponent {
	if traffic.Replicas <= 0 || traffic.LearnedPeakPerReplica <= 0 {
		return ReplicaDecisionComponent{}
	}
	score := 0.0
	influence := "hold"
	if traffic.ScaleUpAllowed {
		score = 0.25
		influence = "pressure"
	} else if traffic.Replicas < current {
		score = -0.12
		influence = "waste"
	}
	return ReplicaDecisionComponent{
		Name:      "traffic_forecast",
		Score:     score,
		Replicas:  traffic.Replicas,
		Basis:     traffic.ForecastBasis,
		Observed:  fmt.Sprintf("forecast=%.4g pressure_multiplier=%.3g", traffic.Forecast, traffic.PressureMultiplier),
		Influence: influence,
	}
}

func anomalyReplicaComponent(signals []SignalReport, signalName, componentName string, current int32) (ReplicaDecisionComponent, bool) {
	for _, signal := range signals {
		if signal.Name != signalName || signal.Sample == nil {
			continue
		}
		score := 0.0
		influence := "hold"
		switch signal.Anomaly.State {
		case "critical":
			score = 0.35
			influence = "pressure"
		case "warning":
			score = 0.18
			influence = "pressure"
		case "normal":
			if signal.History != nil && signal.History.P50 > 0 && *signal.Sample < signal.History.P50 {
				score = -0.05
				influence = "waste"
			}
		}
		replicas := current
		if score >= 0.30 {
			replicas = current + 1
		}
		return ReplicaDecisionComponent{
			Name:      componentName,
			Score:     score,
			Replicas:  replicas,
			Basis:     signal.Anomaly.State,
			Observed:  fmt.Sprintf("current=%s", formatSignalValue(signal.Name, *signal.Sample)),
			Influence: influence,
		}, true
	}
	return ReplicaDecisionComponent{}, false
}

func resourceReplicaComponent(name string, replicas, current int32, hasDemand bool, demand, targetUtilization float64) (ReplicaDecisionComponent, bool) {
	if !hasDemand || replicas <= 0 {
		return ReplicaDecisionComponent{}, false
	}
	score := 0.0
	influence := "hold"
	switch {
	case replicas > current:
		score = 0.30
		influence = "pressure"
	case replicas < current:
		score = -0.10
		influence = "waste"
	}
	return ReplicaDecisionComponent{
		Name:      name + "_pressure",
		Score:     score,
		Replicas:  replicas,
		Basis:     "learned_p95_target",
		Observed:  fmt.Sprintf("demand=%.4g target_utilization=%.2f", demand, targetUtilization),
		Influence: influence,
	}, true
}

func replicaDecisionReason(decision ReplicaDecision) []string {
	reasons := []string{
		fmt.Sprintf("replica_basis:%s", decision.Basis),
		fmt.Sprintf("replica_signal_score:%.3g", decision.Score),
	}
	for _, component := range decision.Components {
		reasons = append(reasons, fmt.Sprintf("replica_signal_component:%s score=%.3g replicas=%d influence=%s basis=%s observed=%s", component.Name, component.Score, component.Replicas, component.Influence, component.Basis, component.Observed))
	}
	return reasons
}

func replicaDecisionBasis(decision ReplicaDecision) string {
	var pressure []string
	var floors []string
	var waste []string
	for _, component := range decision.Components {
		switch component.Influence {
		case "pressure":
			pressure = append(pressure, component.Name)
		case "floor":
			floors = append(floors, component.Name)
		case "waste":
			waste = append(waste, component.Name)
		}
	}
	if len(pressure) > 0 && len(floors) > 0 {
		return strings.Join(pressure, "+") + ", " + strings.Join(floors, "+")
	}
	if len(pressure) > 0 {
		return strings.Join(pressure, "+")
	}
	if len(floors) > 0 {
		return strings.Join(floors, "+")
	}
	if len(waste) > 0 {
		return "low " + strings.Join(waste, "+")
	}
	return "hold"
}

func trafficReplicaDecision(report WorkloadReport, sharedSignals []SignalReport, replicas float64) trafficDecision {
	requestRate, hasRequestRate := signalSample(report.MetricSignals, "request_rate")
	history := signalHistory(report.MetricSignals, "request_rate")
	if !hasRequestRate || history == nil || history.Points < 6 || history.Max <= 0 || replicas <= 0 {
		return trafficDecision{}
	}

	learnedPeakPerReplica := history.Max / replicas
	if learnedPeakPerReplica <= 0 {
		return trafficDecision{}
	}

	baseForecast := math.Max(requestRate, history.P95)
	forecastBasis := "max_current_or_history_p95"
	if horizon, ok := signalForecastHorizon(report.MetricSignals, "request_rate", "next_30m"); ok {
		if horizon.P95BandHigh > baseForecast {
			baseForecast = horizon.P95BandHigh
			forecastBasis = "next_30m_p95_band_high"
		}
	}
	pressureMultiplier := workloadPressureMultiplier(report.MetricSignals) * sharedPressureMultiplier(sharedSignals)
	scaleUpAllowed := trafficScaleUpAllowed(report.MetricSignals, sharedSignals, requestRate, history)
	forecast := baseForecast
	if scaleUpAllowed {
		forecast *= pressureMultiplier
	}

	const targetTrafficHeadroom = 0.75
	replicasNeeded := int32(math.Ceil(forecast / (learnedPeakPerReplica * targetTrafficHeadroom)))
	if !scaleUpAllowed && replicasNeeded > int32(replicas) {
		replicasNeeded = int32(replicas)
	}
	return trafficDecision{
		Replicas:              replicasNeeded,
		Forecast:              forecast,
		ForecastBasis:         forecastBasis,
		LearnedPeakPerReplica: learnedPeakPerReplica,
		PressureMultiplier:    pressureMultiplier,
		ScaleUpAllowed:        scaleUpAllowed,
	}
}

func signalForecastHorizon(signals []SignalReport, name, horizonName string) (ForecastHorizon, bool) {
	for _, signal := range signals {
		if signal.Name != name || signal.Forecast == nil {
			continue
		}
		for _, horizon := range signal.Forecast.Horizons {
			if horizon.Horizon == horizonName {
				return horizon, true
			}
		}
	}
	return ForecastHorizon{}, false
}

func trafficScaleUpAllowed(workloadSignals, sharedSignals []SignalReport, requestRate float64, history *SignalHistory) bool {
	if history == nil || history.Max <= 0 {
		return false
	}
	if requestRate > history.Max || isSignalCritical(workloadSignals, "request_rate") {
		return true
	}
	highTraffic := history.P95 > 0 && requestRate >= history.P95
	if highTraffic && (isSignalPressured(workloadSignals, "latency_p95") || isSignalPressured(workloadSignals, "error_rate")) {
		return true
	}
	if highTraffic && (isSignalPressured(sharedSignals, "request_rate") || isSignalPressured(sharedSignals, "concurrent_requests") || isSignalPressured(sharedSignals, "error_rate")) {
		return true
	}
	return false
}

func workloadPressureMultiplier(signals []SignalReport) float64 {
	multiplier := 1.0
	if isSignalPressured(signals, "latency_p95") {
		multiplier *= 1.15
	}
	if isSignalPressured(signals, "error_rate") {
		multiplier *= 1.20
	}
	if isSignalCritical(signals, "request_rate") {
		multiplier *= 1.35
	}
	return multiplier
}

func sharedPressureMultiplier(signals []SignalReport) float64 {
	multiplier := 1.0
	if isSignalPressured(signals, "request_rate") {
		multiplier *= 1.10
	}
	if isSignalPressured(signals, "concurrent_requests") {
		multiplier *= 1.10
	}
	if isSignalPressured(signals, "error_rate") {
		multiplier *= 1.10
	}
	return multiplier
}

func isSignalPressured(signals []SignalReport, name string) bool {
	for _, signal := range signals {
		if signal.Name != name {
			continue
		}
		if signal.Anomaly.State == "warning" || signal.Anomaly.State == "critical" {
			return true
		}
	}
	return false
}

func isSignalCritical(signals []SignalReport, name string) bool {
	for _, signal := range signals {
		if signal.Name == name && signal.Anomaly.State == "critical" {
			return true
		}
	}
	return false
}

func signalSample(signals []SignalReport, name string) (float64, bool) {
	for _, signal := range signals {
		if signal.Name == name && signal.Sample != nil {
			return *signal.Sample, true
		}
	}
	return 0, false
}

func activeAnomalies(signals []SignalReport) []string {
	var reasons []string
	for _, signal := range signals {
		if signal.Anomaly.State == "warning" || signal.Anomaly.State == "critical" {
			reasons = append(reasons, fmt.Sprintf("%s_anomaly_%s:%s", signal.Name, signal.Anomaly.State, signal.Anomaly.Reason))
		}
	}
	return reasons
}

func hasActiveAnomaly(signals []SignalReport) bool {
	for _, signal := range signals {
		if signal.Anomaly.State == "warning" || signal.Anomaly.State == "critical" {
			return true
		}
	}
	return false
}

func signalHistory(signals []SignalReport, name string) *SignalHistory {
	for _, signal := range signals {
		if signal.Name == name {
			return signal.History
		}
	}
	return nil
}

func classifyAnomaly(signal SignalReport) AnomalyStatus {
	if signal.Sample == nil {
		return AnomalyStatus{State: "unknown", Reason: "no_current_sample"}
	}
	if signal.History == nil || signal.History.Points < 6 {
		return AnomalyStatus{State: "unknown", Reason: "insufficient_history"}
	}
	value := *signal.Sample
	history := signal.History
	if history.Max > 0 && value > history.Max*1.10 {
		return AnomalyStatus{
			State:  "critical",
			Reason: fmt.Sprintf("current %s exceeds historical max %s by more than 10%%", formatSignalValue(signal.Name, value), formatSignalValue(signal.Name, history.Max)),
		}
	}
	if history.Max > 0 && value > history.Max {
		return AnomalyStatus{
			State:  "warning",
			Reason: fmt.Sprintf("current %s exceeds historical max %s", formatSignalValue(signal.Name, value), formatSignalValue(signal.Name, history.Max)),
		}
	}
	if history.P95 > 0 && value > history.P95*1.25 {
		return AnomalyStatus{
			State:  "warning",
			Reason: fmt.Sprintf("current %s exceeds historical p95 %s by more than 25%%", formatSignalValue(signal.Name, value), formatSignalValue(signal.Name, history.P95)),
		}
	}
	return AnomalyStatus{State: "normal"}
}

func summarizeHistory(result *prom.RangeQueryResult, window, step time.Duration) *SignalHistory {
	return summarizeSamples(aggregateRangeSamples(result), window, step)
}

func aggregateRangeSamples(result *prom.RangeQueryResult) []prom.Sample {
	if result == nil {
		return nil
	}
	byTimestamp := map[float64]float64{}
	for _, series := range result.Series {
		for _, sample := range series.Values {
			if !math.IsNaN(sample.Value) && !math.IsInf(sample.Value, 0) {
				byTimestamp[sample.Timestamp] += sample.Value
			}
		}
	}
	samples := make([]prom.Sample, 0, len(byTimestamp))
	for timestamp, value := range byTimestamp {
		samples = append(samples, prom.Sample{Timestamp: timestamp, Value: value})
	}
	sort.Slice(samples, func(i, j int) bool {
		return samples[i].Timestamp < samples[j].Timestamp
	})
	return samples
}

func summarizeSamples(samples []prom.Sample, window, step time.Duration) *SignalHistory {
	var values []float64
	var firstSampleAt, lastSampleAt *time.Time
	for _, sample := range samples {
		if !math.IsNaN(sample.Value) && !math.IsInf(sample.Value, 0) {
			values = append(values, sample.Value)
			sampledAt := time.Unix(0, int64(sample.Timestamp*float64(time.Second))).UTC()
			if firstSampleAt == nil || sampledAt.Before(*firstSampleAt) {
				value := sampledAt
				firstSampleAt = &value
			}
			if lastSampleAt == nil || sampledAt.After(*lastSampleAt) {
				value := sampledAt
				lastSampleAt = &value
			}
		}
	}
	if len(values) == 0 {
		return nil
	}
	sort.Float64s(values)
	expectedPoints := 0
	if step > 0 && window > 0 {
		expectedPoints = int(math.Ceil(float64(window) / float64(step)))
	}
	coverage := 0.0
	if expectedPoints > 0 {
		coverage = math.Min(1, float64(len(values))/float64(expectedPoints))
	}
	return &SignalHistory{
		Window:         formatDuration(window),
		Step:           formatDuration(step),
		Points:         len(values),
		ExpectedPoints: expectedPoints,
		Coverage:       coverage,
		FirstSampleAt:  firstSampleAt,
		LastSampleAt:   lastSampleAt,
		Min:            values[0],
		P50:            percentile(values, 0.50),
		P95:            percentile(values, 0.95),
		Max:            values[len(values)-1],
	}
}

func buildSignalForecast(samples []prom.Sample, current *float64, history SignalHistory) *SignalForecast {
	recent := recentForecastSamples(samples, 24)
	if len(recent) < 3 {
		return &SignalForecast{Reason: "insufficient_recent_samples_for_trend"}
	}
	slope, residuals := linearTrend(recent)
	lastValue := recent[len(recent)-1].Value
	if current != nil && !math.IsNaN(*current) && !math.IsInf(*current, 0) {
		lastValue = *current
	}
	residualP95 := percentileAbs(residuals, 0.95)
	historyBand := math.Max(0, history.P95-history.P50)
	horizons := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour}
	forecast := &SignalForecast{
		TrendSlopePerHour: slope,
	}
	for _, horizon := range horizons {
		hours := horizon.Hours()
		value := math.Max(0, lastValue+slope*hours)
		band := maxFloat64(residualP95, historyBand*0.50, math.Abs(value)*0.10)
		confidence := forecastConfidence(len(recent), band, value)
		forecast.Horizons = append(forecast.Horizons, ForecastHorizon{
			Horizon:     "next_" + formatDuration(horizon),
			Forecast:    value,
			P95BandLow:  math.Max(0, value-band),
			P95BandHigh: value + band,
			Confidence:  confidence,
		})
	}
	return forecast
}

func recentForecastSamples(samples []prom.Sample, limit int) []prom.Sample {
	var filtered []prom.Sample
	for _, sample := range samples {
		if !math.IsNaN(sample.Value) && !math.IsInf(sample.Value, 0) {
			filtered = append(filtered, sample)
		}
	}
	if len(filtered) <= limit {
		return filtered
	}
	return filtered[len(filtered)-limit:]
}

func linearTrend(samples []prom.Sample) (float64, []float64) {
	if len(samples) < 2 {
		return 0, nil
	}
	base := samples[0].Timestamp
	var sumX, sumY float64
	for _, sample := range samples {
		x := (sample.Timestamp - base) / 3600
		sumX += x
		sumY += sample.Value
	}
	meanX := sumX / float64(len(samples))
	meanY := sumY / float64(len(samples))
	var numerator, denominator float64
	for _, sample := range samples {
		x := (sample.Timestamp - base) / 3600
		numerator += (x - meanX) * (sample.Value - meanY)
		denominator += (x - meanX) * (x - meanX)
	}
	slope := 0.0
	if denominator > 0 {
		slope = numerator / denominator
	}
	intercept := meanY - slope*meanX
	residuals := make([]float64, 0, len(samples))
	for _, sample := range samples {
		x := (sample.Timestamp - base) / 3600
		predicted := intercept + slope*x
		residuals = append(residuals, sample.Value-predicted)
	}
	return slope, residuals
}

func percentileAbs(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	absolute := make([]float64, 0, len(values))
	for _, value := range values {
		if !math.IsNaN(value) && !math.IsInf(value, 0) {
			absolute = append(absolute, math.Abs(value))
		}
	}
	if len(absolute) == 0 {
		return 0
	}
	sort.Float64s(absolute)
	return percentile(absolute, quantile)
}

func forecastConfidence(points int, band, forecast float64) float64 {
	pointScore := math.Min(float64(points)/24, 1) * 0.25
	relativeBand := band / math.Max(1, math.Abs(forecast))
	bandPenalty := math.Min(relativeBand, 0.45)
	return clampFloat(0.65+pointScore-bandPenalty, 0.05, 0.99)
}

func maxFloat64(values ...float64) float64 {
	if len(values) == 0 {
		return 0
	}
	maximum := values[0]
	for _, value := range values[1:] {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func formatDuration(duration time.Duration) string {
	if duration%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(duration/time.Hour))
	}
	if duration%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(duration/time.Minute))
	}
	return duration.String()
}

func percentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := quantile * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func recommendationConfidence(report WorkloadReport, hasCPU, hasMemory, hasTraffic bool) float64 {
	confidence := 0.4
	if report.MetricsCondition == "healthy" {
		confidence += 0.3
	}
	if hasTraffic {
		confidence += 0.1
	}
	if hasCPU {
		confidence += 0.1
	}
	if hasMemory {
		confidence += 0.1
	}
	if !report.CommitBlocked {
		confidence += 0.1
	}
	return math.Min(confidence, 0.95)
}

func applyConfidenceDecay(workload config.WorkloadSpec, report WorkloadReport, sharedSignals []SignalReport, recommendation *Recommendation, baseConfidence float64) {
	assessment := ConfidenceAssessment{
		Base:              roundFloat(baseConfidence, 3),
		Adjusted:          roundFloat(baseConfidence, 3),
		MinAutoCommit:     confidenceMinAutoCommit(workload.Policy.Confidence),
		AutoCommitAllowed: true,
	}
	for _, factor := range confidenceSignalFactors(report.MetricSignals, time.Now().UTC()) {
		assessment.Signals = append(assessment.Signals, factor)
		assessment.Decay += factor.Decay
		if factor.Decay > 0 {
			assessment.Reasons = append(assessment.Reasons, factor.Name+": "+factor.Reason)
		}
	}
	for _, factor := range confidenceSignalFactors(sharedSignals, time.Now().UTC()) {
		assessment.Signals = append(assessment.Signals, factor)
		assessment.Decay += factor.Decay
		if factor.Decay > 0 {
			assessment.Reasons = append(assessment.Reasons, "shared_"+factor.Name+": "+factor.Reason)
		}
	}
	assessment.Decay = roundFloat(math.Min(0.6, assessment.Decay), 3)
	assessment.Adjusted = roundFloat(clampFloat(baseConfidence-assessment.Decay, 0, 0.95), 3)
	assessment.AutoCommitAllowed = assessment.Adjusted >= assessment.MinAutoCommit
	if assessment.AutoCommitAllowed && len(assessment.Reasons) == 0 {
		assessment.Reasons = append(assessment.Reasons, "prometheus signal quality supports confidence")
	}
	recommendation.Confidence = assessment.Adjusted
	recommendation.ConfidenceAssessment = assessment
	if !assessment.AutoCommitAllowed && recommendationHasChange(*recommendation) {
		reason := fmt.Sprintf("confidence %.2f below minAutoCommit %.2f blocks auto-commit", assessment.Adjusted, assessment.MinAutoCommit)
		recommendation.Blocked = true
		if !stringSliceContains(recommendation.BlockReasons, reason) {
			recommendation.BlockReasons = append(recommendation.BlockReasons, reason)
		}
		if !stringSliceContains(recommendation.ReasonCodes, "confidence_below_min_auto_commit") {
			recommendation.ReasonCodes = append(recommendation.ReasonCodes, "confidence_below_min_auto_commit")
		}
	}
}

func confidenceMinAutoCommit(policy config.ConfidencePolicySpec) float64 {
	if policy.MinAutoCommit > 0 {
		return policy.MinAutoCommit
	}
	return 0.75
}

func confidenceSignalFactors(signals []SignalReport, now time.Time) []SignalConfidenceFactor {
	var factors []SignalConfidenceFactor
	for _, signal := range signals {
		if !confidenceSignalRelevant(signal) {
			continue
		}
		factors = append(factors, confidenceSignalFactor(signal, now))
	}
	return factors
}

func confidenceSignalRelevant(signal SignalReport) bool {
	if signal.Required {
		return true
	}
	switch signal.Name {
	case "request_rate", "latency_p95", "error_rate", "concurrent_requests", "cpu_usage", "memory_working_set":
		return signal.Sample != nil || signal.History != nil || signal.HistoryError != "" || signal.Error != ""
	default:
		return false
	}
}

func confidenceSignalFactor(signal SignalReport, now time.Time) SignalConfidenceFactor {
	factor := SignalConfidenceFactor{Name: signal.Name, Required: signal.Required, Quality: "ok"}
	switch {
	case signal.Error != "":
		factor.Quality = "stale"
		factor.Decay = confidenceDecay(signal.Required, 0.20, 0.08)
		factor.Reason = "current query error: " + signal.Error
		return factor
	case signal.Sample == nil:
		factor.Quality = "stale"
		factor.Decay = confidenceDecay(signal.Required, 0.18, 0.06)
		factor.Reason = "no current sample"
		return factor
	case signal.HistoryError != "":
		factor.Quality = "sparse"
		factor.Decay = confidenceDecay(signal.Required, 0.14, 0.05)
		factor.Reason = "history query error: " + signal.HistoryError
		return factor
	case signal.History == nil:
		factor.Quality = "sparse"
		factor.Decay = confidenceDecay(signal.Required, 0.16, 0.05)
		factor.Reason = "no range history"
		return factor
	}
	history := signal.History
	switch {
	case history.ExpectedPoints > 0 && history.Coverage < 0.50:
		factor.Quality = "sparse"
		factor.Decay = confidenceDecay(signal.Required, 0.14, 0.05)
		factor.Reason = fmt.Sprintf("history coverage %.0f%% with %d/%d points", history.Coverage*100, history.Points, history.ExpectedPoints)
	case history.Points < 6:
		factor.Quality = "sparse"
		factor.Decay = confidenceDecay(signal.Required, 0.12, 0.04)
		factor.Reason = fmt.Sprintf("only %d history points", history.Points)
	case historyStale(history, now):
		factor.Quality = "stale"
		factor.Decay = confidenceDecay(signal.Required, 0.16, 0.06)
		factor.Reason = fmt.Sprintf("last history sample at %s", history.LastSampleAt.Format(time.RFC3339))
	case historyNoisy(*history, signal.Forecast):
		factor.Quality = "noisy"
		factor.Decay = confidenceDecay(signal.Required, 0.10, 0.04)
		factor.Reason = "history or forecast band is noisy"
	default:
		if history.ExpectedPoints > 0 {
			factor.Reason = fmt.Sprintf("history coverage %.0f%% with %d points", history.Coverage*100, history.Points)
		} else {
			factor.Reason = fmt.Sprintf("history has %d points", history.Points)
		}
	}
	factor.Decay = roundFloat(factor.Decay, 3)
	return factor
}

func confidenceDecay(required bool, requiredDecay, optionalDecay float64) float64 {
	if required {
		return requiredDecay
	}
	return optionalDecay
}

func historyStale(history *SignalHistory, now time.Time) bool {
	if history == nil || history.LastSampleAt == nil || now.IsZero() {
		return false
	}
	step, err := time.ParseDuration(history.Step)
	if err != nil || step <= 0 {
		step = 5 * time.Minute
	}
	staleAfter := maxDuration(3*step, 15*time.Minute)
	return now.Sub(*history.LastSampleAt) > staleAfter
}

func historyNoisy(history SignalHistory, forecast *SignalForecast) bool {
	if history.P50 > 0.001 && (history.P95-history.P50)/math.Abs(history.P50) > 1.5 {
		return true
	}
	if history.P95 > 0.001 && history.Max/history.P95 > 3 {
		return true
	}
	if forecast != nil {
		for _, horizon := range forecast.Horizons {
			if horizon.Confidence > 0 && horizon.Confidence < 0.50 {
				return true
			}
		}
	}
	return false
}

func recommendationHasChange(recommendation Recommendation) bool {
	return recommendation.RecommendedReplicas != recommendation.CurrentReplicas ||
		(recommendation.CurrentCPURequest != "" && recommendation.RecommendedCPURequest != "" && recommendation.RecommendedCPURequest != recommendation.CurrentCPURequest) ||
		(recommendation.CurrentMemoryRequest != "" && recommendation.RecommendedMemoryRequest != "" && recommendation.RecommendedMemoryRequest != recommendation.CurrentMemoryRequest)
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func roundFloat(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}

type resourcePolicyProfile struct {
	MinTargetUtilization float64
	MaxTargetUtilization float64
	MinIncreasePercent   int
	MaxIncreasePercent   int
	MinDecreasePercent   int
	MaxDecreasePercent   int
	IncreaseGap          float64
	IncreaseVolatility   float64
	DecreaseGap          float64
	DecreaseVolatility   float64
}

type resourcePolicy struct {
	TargetUtilization  float64
	IncreaseThreshold  float64
	DecreaseThreshold  float64
	MaxIncreasePercent int
	MaxDecreasePercent int
	Volatility         float64
	HistoryPoints      int
	Guardrails         []string
}

func cpuPolicyProfile() resourcePolicyProfile {
	return resourcePolicyProfile{
		MinTargetUtilization: 0.55,
		MaxTargetUtilization: 0.72,
		MinIncreasePercent:   35,
		MaxIncreasePercent:   125,
		MinDecreasePercent:   10,
		MaxDecreasePercent:   45,
		IncreaseGap:          0.08,
		IncreaseVolatility:   0.10,
		DecreaseGap:          0.15,
		DecreaseVolatility:   0.15,
	}
}

func memoryPolicyProfile() resourcePolicyProfile {
	return resourcePolicyProfile{
		MinTargetUtilization: 0.70,
		MaxTargetUtilization: 0.84,
		MinIncreasePercent:   20,
		MaxIncreasePercent:   75,
		MinDecreasePercent:   5,
		MaxDecreasePercent:   35,
		IncreaseGap:          0.08,
		IncreaseVolatility:   0.08,
		DecreaseGap:          0.16,
		DecreaseVolatility:   0.18,
	}
}

func learnedResourcePolicy(signals []SignalReport, signalName string, profile resourcePolicyProfile, guardrail config.ChangeBounds) resourcePolicy {
	policy := resourcePolicy{
		Volatility:    0.65,
		HistoryPoints: 0,
	}
	if history := signalHistory(signals, signalName); history != nil && history.Points >= 12 && history.P95 > 0 {
		spread := clampFloat((history.P95-history.P50)/history.P95, 0, 1)
		burst := clampFloat((history.Max-history.P95)/history.P95, 0, 1)
		policy.Volatility = clampFloat((spread*0.7)+(burst*0.3), 0, 1)
		policy.HistoryPoints = history.Points
	}

	policy.TargetUtilization = profile.MaxTargetUtilization - ((profile.MaxTargetUtilization - profile.MinTargetUtilization) * policy.Volatility)
	policy.IncreaseThreshold = clampFloat(policy.TargetUtilization+profile.IncreaseGap+(profile.IncreaseVolatility*policy.Volatility), policy.TargetUtilization, 0.95)
	policy.DecreaseThreshold = clampFloat(policy.TargetUtilization-profile.DecreaseGap-(profile.DecreaseVolatility*policy.Volatility), 0.05, policy.TargetUtilization)
	policy.MaxIncreasePercent = percentFromVolatility(profile.MinIncreasePercent, profile.MaxIncreasePercent, policy.Volatility)
	policy.MaxDecreasePercent = percentFromVolatility(profile.MaxDecreasePercent, profile.MinDecreasePercent, policy.Volatility)

	if guardrail.MaxIncreasePercent > 0 && policy.MaxIncreasePercent > guardrail.MaxIncreasePercent {
		policy.MaxIncreasePercent = guardrail.MaxIncreasePercent
		policy.Guardrails = append(policy.Guardrails, fmt.Sprintf("max_up_guardrail=%d%%", guardrail.MaxIncreasePercent))
	}
	if guardrail.MaxDecreasePercent > 0 && policy.MaxDecreasePercent > guardrail.MaxDecreasePercent {
		policy.MaxDecreasePercent = guardrail.MaxDecreasePercent
		policy.Guardrails = append(policy.Guardrails, fmt.Sprintf("max_down_guardrail=%d%%", guardrail.MaxDecreasePercent))
	}
	return policy
}

func learnedPolicyReason(name string, policy resourcePolicy) string {
	parts := []string{
		fmt.Sprintf("target=%.0f%%", policy.TargetUtilization*100),
		fmt.Sprintf("increase_after=%.0f%%", policy.IncreaseThreshold*100),
		fmt.Sprintf("decrease_below=%.0f%%", policy.DecreaseThreshold*100),
		fmt.Sprintf("max_up=%d%%", policy.MaxIncreasePercent),
		fmt.Sprintf("max_down=%d%%", policy.MaxDecreasePercent),
		fmt.Sprintf("volatility=%.2f", policy.Volatility),
		fmt.Sprintf("points=%d", policy.HistoryPoints),
	}
	parts = append(parts, policy.Guardrails...)
	return name + ":" + strings.Join(parts, ",")
}

func percentFromVolatility(atLowVolatility, atHighVolatility int, volatility float64) int {
	value := float64(atLowVolatility) + ((float64(atHighVolatility - atLowVolatility)) * clampFloat(volatility, 0, 1))
	return int(math.Round(value))
}

func clampFloat(value, minValue, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func boundedCPURequest(currentCores, recommendedCores float64, maxIncreasePercent, maxDecreasePercent int) string {
	if maxIncreasePercent > 0 {
		maxCores := currentCores * (1 + float64(maxIncreasePercent)/100)
		recommendedCores = math.Min(recommendedCores, maxCores)
	}
	if maxDecreasePercent > 0 {
		minCores := currentCores * (1 - float64(maxDecreasePercent)/100)
		recommendedCores = math.Max(recommendedCores, minCores)
	}
	milli := int64(math.Ceil(recommendedCores*1000/10) * 10)
	if milli < 1 {
		milli = 1
	}
	return fmt.Sprintf("%dm", milli)
}

func boundedMemoryRequest(currentBytes, recommendedBytes float64, maxIncreasePercent, maxDecreasePercent int) string {
	if maxIncreasePercent > 0 {
		maxBytes := currentBytes * (1 + float64(maxIncreasePercent)/100)
		recommendedBytes = math.Min(recommendedBytes, maxBytes)
	}
	if maxDecreasePercent > 0 {
		minBytes := currentBytes * (1 - float64(maxDecreasePercent)/100)
		recommendedBytes = math.Max(recommendedBytes, minBytes)
	}
	mi := int64(math.Ceil(recommendedBytes / (1024 * 1024)))
	if mi < 1 {
		mi = 1
	}
	return fmt.Sprintf("%dMi", mi)
}

func maxInt32(values ...int32) int32 {
	var maxValue int32
	for _, value := range values {
		if value > maxValue {
			maxValue = value
		}
	}
	return maxValue
}

func metricCondition(signals []SignalReport) string {
	required := 0
	requiredHealthy := 0
	optionalHealthy := 0
	for _, signal := range signals {
		if signal.Required {
			required++
			if signal.Healthy {
				requiredHealthy++
			}
		} else if signal.Healthy {
			optionalHealthy++
		}
	}
	if required > 0 && requiredHealthy == required {
		return "healthy"
	}
	if required == 0 && optionalHealthy > 0 {
		return "healthy"
	}
	if requiredHealthy > 0 || optionalHealthy > 0 {
		return "degraded"
	}
	return "unhealthy"
}
