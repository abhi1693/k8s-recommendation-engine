package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	recommendationv1alpha1 "github.com/abhi1693/k8s-recommendation-engine/api/v1alpha1"
	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcileProcessesProfileAndUpdatesStatus(t *testing.T) {
	resource := validProfileResource()
	resource.Generation = 3
	interval := 7 * time.Minute
	resource.Spec.ReconcileInterval = &metav1.Duration{Duration: interval}
	processor := &fakeProcessor{report: healthyReport()}
	reconciler, kubeClient := testReconciler(t, resource, processor)
	now := time.Date(2026, 7, 12, 16, 0, 0, 0, time.UTC)
	reconciler.Now = func() time.Time { return now }

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(resource)})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != interval {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, interval)
	}
	if processor.calls.Load() != 1 {
		t.Fatalf("processor calls = %d, want 1", processor.calls.Load())
	}
	if processor.profile == nil || processor.profile.Metadata.Name != "cluster-ops/shipyard" {
		t.Fatalf("converted profile = %#v, want namespaced profile identity", processor.profile)
	}
	if got := processor.profile.Spec.Workloads[0].Policy.AvailabilityRecovery.FailureGracePeriod; got != "2m0s" {
		t.Fatalf("converted failure grace period = %q, want 2m0s", got)
	}

	updated := getProfile(t, kubeClient, resource)
	if updated.Status.ObservedGeneration != 3 || updated.Status.LastSuccessfulTime == nil || !updated.Status.LastSuccessfulTime.Time.Equal(now) {
		t.Fatalf("status timestamps/generation = %#v", updated.Status)
	}
	if updated.Status.Summary.Healthy != 1 || len(updated.Status.Workloads) != 1 {
		t.Fatalf("status summary = %#v workloads=%#v", updated.Status.Summary, updated.Status.Workloads)
	}
	if updated.Status.Proposal == nil || !updated.Status.Proposal.Pushed || updated.Status.Proposal.Commit != "abc123" {
		t.Fatalf("proposal status = %#v", updated.Status.Proposal)
	}
	if updated.Status.Git == nil || updated.Status.Git.Status != "healthy" {
		t.Fatalf("git status = %#v", updated.Status.Git)
	}
	ready := apiMeta.FindStatusCondition(updated.Status.Conditions, ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != "Reconciled" {
		t.Fatalf("Ready condition = %#v", ready)
	}
}

func TestProfileConfigUsesExplicitMigrationStateKey(t *testing.T) {
	resource := validProfileResource()
	resource.Spec.StateKey = "shipyard"
	profile, err := profileConfig(resource)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Metadata.Name != "shipyard" {
		t.Fatalf("profile state identity = %q, want shipyard", profile.Metadata.Name)
	}
}

func TestReconcileSuspendedProfileDoesNotProcess(t *testing.T) {
	resource := validProfileResource()
	resource.Spec.Suspend = true
	processor := &fakeProcessor{report: healthyReport()}
	reconciler, kubeClient := testReconciler(t, resource, processor)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(resource)})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 || processor.calls.Load() != 0 {
		t.Fatalf("result=%#v processor calls=%d, want suspended", result, processor.calls.Load())
	}
	updated := getProfile(t, kubeClient, resource)
	suspended := apiMeta.FindStatusCondition(updated.Status.Conditions, ConditionSuspended)
	if suspended == nil || suspended.Status != metav1.ConditionTrue {
		t.Fatalf("Suspended condition = %#v", suspended)
	}
}

func TestReconcileRecordsProcessorFailure(t *testing.T) {
	resource := validProfileResource()
	processorErr := errors.New("prometheus unavailable")
	processor := &fakeProcessor{err: processorErr}
	reconciler, kubeClient := testReconciler(t, resource, processor)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(resource)})
	if !errors.Is(err, processorErr) {
		t.Fatalf("Reconcile error = %v, want processor error", err)
	}
	updated := getProfile(t, kubeClient, resource)
	ready := apiMeta.FindStatusCondition(updated.Status.Conditions, ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "ReconcileFailed" {
		t.Fatalf("Ready condition = %#v", ready)
	}
}

func TestReconcileInvalidProfileWaitsForSpecChange(t *testing.T) {
	resource := validProfileResource()
	resource.Spec.Workloads[0].MetricProfileRef = "missing"
	processor := &fakeProcessor{report: healthyReport()}
	reconciler, kubeClient := testReconciler(t, resource, processor)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(resource)})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 0 || processor.calls.Load() != 0 {
		t.Fatalf("result=%#v processor calls=%d, want validation hold", result, processor.calls.Load())
	}
	updated := getProfile(t, kubeClient, resource)
	ready := apiMeta.FindStatusCondition(updated.Status.Conditions, ConditionReady)
	if ready == nil || ready.Reason != "InvalidSpec" {
		t.Fatalf("Ready condition = %#v", ready)
	}
}

type fakeProcessor struct {
	report  *analyzer.Report
	err     error
	calls   atomic.Int32
	profile *config.ApplicationProfile
}

func (p *fakeProcessor) Process(_ context.Context, profile *config.ApplicationProfile) (*analyzer.Report, error) {
	p.calls.Add(1)
	p.profile = profile
	return p.report, p.err
}

func testReconciler(t *testing.T, resource *recommendationv1alpha1.ApplicationProfile, processor ProfileProcessor) (*ApplicationProfileReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := recommendationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&recommendationv1alpha1.ApplicationProfile{}).
		WithObjects(resource).
		Build()
	return &ApplicationProfileReconciler{
		Client:                   kubeClient,
		Scheme:                   scheme,
		Processor:                processor,
		DefaultReconcileInterval: 5 * time.Minute,
		ReconcileTimeout:         time.Minute,
	}, kubeClient
}

func getProfile(t *testing.T, kubeClient client.Client, resource *recommendationv1alpha1.ApplicationProfile) *recommendationv1alpha1.ApplicationProfile {
	t.Helper()
	var updated recommendationv1alpha1.ApplicationProfile
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, &updated); err != nil {
		t.Fatal(err)
	}
	return &updated
}

func validProfileResource() *recommendationv1alpha1.ApplicationProfile {
	return &recommendationv1alpha1.ApplicationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "shipyard", Namespace: "cluster-ops"},
		Spec: recommendationv1alpha1.ApplicationProfileSpec{
			Namespace: "shipyardhq",
			Workloads: []recommendationv1alpha1.WorkloadSpec{
				{
					Name:             "web",
					TargetRef:        recommendationv1alpha1.TargetRef{APIVersion: "apps/v1", Kind: "Deployment", Name: "shipyardhq"},
					MetricProfileRef: "resources",
					Policy: recommendationv1alpha1.PolicySpec{
						AvailabilityRecovery: recommendationv1alpha1.AvailabilityRecoveryPolicySpec{
							Enabled:            true,
							FailureGracePeriod: &metav1.Duration{Duration: 2 * time.Minute},
						},
					},
				},
			},
			MetricProfiles: map[string]recommendationv1alpha1.MetricProfile{
				"resources": {Signals: map[string]recommendationv1alpha1.Signal{"cpu_usage": {Query: "up"}}},
			},
		},
	}
}

func healthyReport() *analyzer.Report {
	return &analyzer.Report{
		Application: "cluster-ops/shipyard",
		Proposal:    &analyzer.ProposalReport{Needed: true, Pushed: true, Commit: "abc123"},
		GitHealth:   &analyzer.GitHealthReport{Status: "healthy", Branch: "master"},
		Summary: analyzer.Summary{
			WorkloadsTotal: 1,
			MetricsHealthy: 1,
		},
		Workloads: []analyzer.WorkloadReport{
			{
				Name:             "web",
				Namespace:        "shipyardhq",
				Deployment:       "shipyardhq",
				ReadyReplicas:    3,
				MetricsCondition: "healthy",
				Recommendation: analyzer.Recommendation{
					CurrentReplicas:          3,
					RecommendedReplicas:      3,
					CurrentCPURequest:        "200m",
					RecommendedCPURequest:    "200m",
					CurrentMemoryRequest:     "3110Mi",
					RecommendedMemoryRequest: "3110Mi",
					Confidence:               0.9,
					Safety:                   analyzer.SafetyAssessment{Classification: analyzer.SafetyLowRisk},
				},
			},
		},
	}
}
