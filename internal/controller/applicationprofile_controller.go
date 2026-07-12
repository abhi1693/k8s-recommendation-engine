package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	recommendationv1alpha1 "github.com/abhi1693/k8s-recommendation-engine/api/v1alpha1"
	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerRuntime "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	ConditionReady     = "Ready"
	ConditionDegraded  = "Degraded"
	ConditionSuspended = "Suspended"
)

// ApplicationProfileReconciler reconciles ApplicationProfile resources.
type ApplicationProfileReconciler struct {
	client.Client
	Scheme                   *runtime.Scheme
	Recorder                 record.EventRecorder
	Processor                ProfileProcessor
	DefaultReconcileInterval time.Duration
	ReconcileTimeout         time.Duration
	MaxConcurrentReconciles  int
	Now                      func() time.Time
}

// +kubebuilder:rbac:groups=k8s-recommendation-engine.io,resources=applicationprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s-recommendation-engine.io,resources=applicationprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=pods;services,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch
// +kubebuilder:rbac:urls=/api;/api/*;/apis;/apis/*,verbs=get

func (r *ApplicationProfileReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var resource recommendationv1alpha1.ApplicationProfile
	if err := r.Get(ctx, request.NamespacedName, &resource); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	now := r.now()
	interval, err := r.reconcileInterval(&resource)
	if err != nil {
		return ctrl.Result{}, r.updateFailureStatus(ctx, &resource, now, "InvalidSpec", err)
	}
	if resource.Spec.Suspend {
		return ctrl.Result{}, r.updateSuspendedStatus(ctx, &resource, now)
	}

	profile, err := profileConfig(&resource)
	if err != nil {
		if r.Recorder != nil {
			r.Recorder.Eventf(&resource, "Warning", "InvalidSpec", "Profile validation failed: %v", err)
		}
		return ctrl.Result{}, r.updateFailureStatus(ctx, &resource, now, "InvalidSpec", err)
	}
	if r.Processor == nil {
		err = fmt.Errorf("profile processor is not configured")
		return ctrl.Result{}, r.updateFailureStatus(ctx, &resource, now, "ConfigurationError", err)
	}

	processCtx := ctx
	cancel := func() {}
	if r.ReconcileTimeout > 0 {
		processCtx, cancel = context.WithTimeout(ctx, r.ReconcileTimeout)
	}
	report, processErr := r.Processor.Process(processCtx, profile)
	cancel()
	if processErr != nil {
		statusErr := r.updateFailureStatus(ctx, &resource, now, "ReconcileFailed", processErr)
		if r.Recorder != nil {
			r.Recorder.Eventf(&resource, "Warning", "ReconcileFailed", "Profile reconciliation failed: %v", processErr)
		}
		return ctrl.Result{}, errors.Join(processErr, statusErr)
	}
	if err := r.updateSuccessStatus(ctx, &resource, report, now, interval); err != nil {
		return ctrl.Result{}, err
	}
	r.recordRecoveryEvents(&resource, report)
	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *ApplicationProfileReconciler) SetupWithManager(manager ctrl.Manager) error {
	if r.Processor == nil {
		return fmt.Errorf("profile processor is required")
	}
	concurrency := r.MaxConcurrentReconciles
	if concurrency <= 0 {
		concurrency = 1
	}
	return ctrl.NewControllerManagedBy(manager).
		Named("applicationprofile").
		For(&recommendationv1alpha1.ApplicationProfile{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controllerRuntime.Options{MaxConcurrentReconciles: concurrency}).
		Complete(r)
}

func (r *ApplicationProfileReconciler) reconcileInterval(resource *recommendationv1alpha1.ApplicationProfile) (time.Duration, error) {
	interval := r.DefaultReconcileInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if resource.Spec.ReconcileInterval != nil {
		interval = resource.Spec.ReconcileInterval.Duration
	}
	if interval <= 0 {
		return 0, fmt.Errorf("spec.reconcileInterval must be greater than zero")
	}
	return interval, nil
}

func (r *ApplicationProfileReconciler) updateSuccessStatus(ctx context.Context, resource *recommendationv1alpha1.ApplicationProfile, report *analyzer.Report, now time.Time, interval time.Duration) error {
	base := resource.DeepCopy()
	nowTime := metav1.NewTime(now)
	nextTime := metav1.NewTime(now.Add(interval))
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.LastAttemptTime = &nowTime
	resource.Status.LastSuccessfulTime = &nowTime
	resource.Status.NextReconcileTime = &nextTime
	resource.Status.Summary, resource.Status.Workloads = reportStatus(report)
	resource.Status.Proposal = proposalStatus(report)
	resource.Status.Git = gitStatus(report)
	degraded := resource.Status.Summary.Degraded > 0 || resource.Status.Summary.Unhealthy > 0 || resource.Status.Summary.Emergencies > 0
	setCondition(&resource.Status, resource.Generation, ConditionReady, metav1.ConditionTrue, "Reconciled", "Latest profile reconciliation completed", now)
	setCondition(&resource.Status, resource.Generation, ConditionSuspended, metav1.ConditionFalse, "Active", "Profile reconciliation is active", now)
	if degraded {
		setCondition(&resource.Status, resource.Generation, ConditionDegraded, metav1.ConditionTrue, "WorkloadsDegraded", "One or more workloads require attention", now)
	} else {
		setCondition(&resource.Status, resource.Generation, ConditionDegraded, metav1.ConditionFalse, "WorkloadsHealthy", "All workload observations are healthy", now)
	}
	return r.Status().Patch(ctx, resource, client.MergeFrom(base))
}

func (r *ApplicationProfileReconciler) updateFailureStatus(ctx context.Context, resource *recommendationv1alpha1.ApplicationProfile, now time.Time, reason string, reconcileErr error) error {
	base := resource.DeepCopy()
	nowTime := metav1.NewTime(now)
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.LastAttemptTime = &nowTime
	resource.Status.NextReconcileTime = nil
	setCondition(&resource.Status, resource.Generation, ConditionReady, metav1.ConditionFalse, reason, reconcileErr.Error(), now)
	setCondition(&resource.Status, resource.Generation, ConditionDegraded, metav1.ConditionTrue, reason, reconcileErr.Error(), now)
	setCondition(&resource.Status, resource.Generation, ConditionSuspended, metav1.ConditionFalse, "Active", "Profile reconciliation is active", now)
	return r.Status().Patch(ctx, resource, client.MergeFrom(base))
}

func (r *ApplicationProfileReconciler) updateSuspendedStatus(ctx context.Context, resource *recommendationv1alpha1.ApplicationProfile, now time.Time) error {
	base := resource.DeepCopy()
	nowTime := metav1.NewTime(now)
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.LastAttemptTime = &nowTime
	resource.Status.NextReconcileTime = nil
	setCondition(&resource.Status, resource.Generation, ConditionReady, metav1.ConditionFalse, "Suspended", "Profile reconciliation is suspended", now)
	setCondition(&resource.Status, resource.Generation, ConditionDegraded, metav1.ConditionFalse, "Suspended", "Profile reconciliation is suspended", now)
	setCondition(&resource.Status, resource.Generation, ConditionSuspended, metav1.ConditionTrue, "Suspended", "Profile reconciliation is suspended", now)
	return r.Status().Patch(ctx, resource, client.MergeFrom(base))
}

func (r *ApplicationProfileReconciler) recordRecoveryEvents(resource *recommendationv1alpha1.ApplicationProfile, report *analyzer.Report) {
	if r.Recorder == nil || report == nil {
		return
	}
	for _, workload := range report.Workloads {
		if workload.Recovery == nil || !workload.Recovery.Attempted {
			continue
		}
		if workload.Recovery.Succeeded {
			r.Recorder.Eventf(resource, "Normal", "AvailabilityRecovered", "Recreated failed pod %s for workload %s", workload.Recovery.Pod, workload.Name)
			continue
		}
		r.Recorder.Eventf(resource, "Warning", "AvailabilityRecoveryFailed", "Failed to recreate pod %s for workload %s: %s", workload.Recovery.Pod, workload.Name, workload.Recovery.Error)
	}
}

func (r *ApplicationProfileReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func setCondition(status *recommendationv1alpha1.ApplicationProfileStatus, generation int64, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string, now time.Time) {
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(now),
		Reason:             reason,
		Message:            message,
	})
}

func reportStatus(report *analyzer.Report) (recommendationv1alpha1.ApplicationProfileSummary, []recommendationv1alpha1.WorkloadStatus) {
	if report == nil {
		return recommendationv1alpha1.ApplicationProfileSummary{}, nil
	}
	summary := recommendationv1alpha1.ApplicationProfileSummary{
		Workloads: int32(len(report.Workloads)),
		Healthy:   int32(report.Summary.MetricsHealthy),
		Degraded:  int32(report.Summary.MetricsDegraded),
		Unhealthy: int32(report.Summary.MetricsUnhealthy),
	}
	workloads := make([]recommendationv1alpha1.WorkloadStatus, 0, len(report.Workloads))
	for _, workload := range report.Workloads {
		if workload.Recommendation.Blocked {
			summary.Blocked++
		}
		if workload.Availability.Emergency {
			summary.Emergencies++
		}
		item := recommendationv1alpha1.WorkloadStatus{
			Name:                     workload.Name,
			Target:                   workload.Namespace + "/" + workload.Deployment,
			CurrentReplicas:          workload.Recommendation.CurrentReplicas,
			ReadyReplicas:            workload.ReadyReplicas,
			RecommendedReplicas:      workload.Recommendation.RecommendedReplicas,
			CurrentCPURequest:        workload.Recommendation.CurrentCPURequest,
			RecommendedCPURequest:    workload.Recommendation.RecommendedCPURequest,
			CurrentMemoryRequest:     workload.Recommendation.CurrentMemoryRequest,
			RecommendedMemoryRequest: workload.Recommendation.RecommendedMemoryRequest,
			MetricsCondition:         workload.MetricsCondition,
			Confidence:               workload.Recommendation.Confidence,
			Safety:                   workload.Recommendation.Safety.Classification,
			Blocked:                  workload.Recommendation.Blocked,
			Emergency:                workload.Availability.Emergency,
		}
		if workload.Recovery != nil {
			item.Recovery = &recommendationv1alpha1.RecoveryStatus{
				Attempted: workload.Recovery.Attempted,
				Succeeded: workload.Recovery.Succeeded,
				Action:    workload.Recovery.Action,
				Pod:       workload.Recovery.Pod,
				Reason:    workload.Recovery.Reason,
				Error:     workload.Recovery.Error,
			}
		}
		workloads = append(workloads, item)
	}
	return summary, workloads
}

func proposalStatus(report *analyzer.Report) *recommendationv1alpha1.ProposalStatus {
	if report == nil || report.Proposal == nil {
		return nil
	}
	return &recommendationv1alpha1.ProposalStatus{
		Needed:       report.Proposal.Needed,
		Blocked:      report.Proposal.Blocked,
		Pushed:       report.Proposal.Pushed,
		Branch:       report.Proposal.Branch,
		Commit:       report.Proposal.Commit,
		Message:      report.Proposal.Message,
		BlockReasons: append([]string(nil), report.Proposal.BlockReasons...),
		Errors:       append([]string(nil), report.Proposal.Errors...),
	}
}

func gitStatus(report *analyzer.Report) *recommendationv1alpha1.GitStatus {
	if report == nil || report.GitHealth == nil {
		return nil
	}
	return &recommendationv1alpha1.GitStatus{
		Status:   report.GitHealth.Status,
		Branch:   report.GitHealth.Branch,
		Ahead:    int32(report.GitHealth.Ahead),
		Behind:   int32(report.GitHealth.Behind),
		Diverged: report.GitHealth.Diverged,
		Dirty:    report.GitHealth.Dirty,
		Errors:   append([]string(nil), report.GitHealth.Errors...),
	}
}
