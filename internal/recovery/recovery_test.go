package recovery

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"github.com/abhi1693/k8s-recommendation-engine/internal/kube"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestApplyRecreatesOneFailedPodAndHonorsCooldown(t *testing.T) {
	now := time.Date(2026, 7, 12, 14, 30, 0, 0, time.UTC)
	first := failedPod("shipyardhq-1", "uid-1", now.Add(-10*time.Minute))
	second := failedPod("shipyardhq-2", "uid-2", now.Add(-9*time.Minute))
	clientset := fake.NewSimpleClientset(first, second)
	client := kube.NewFakeClient(clientset)
	profile := recoveryProfile()
	stateDB := filepath.Join(t.TempDir(), "state.db")

	report := recoveryReport(first, now.Add(-10*time.Minute))
	if err := Apply(context.Background(), client, profile, report, Options{Enabled: true, StateDB: stateDB, Now: now}); err != nil {
		t.Fatal(err)
	}
	result := report.Workloads[0].Recovery
	if result == nil || !result.Attempted || !result.Succeeded || result.Pod != first.Name {
		t.Fatalf("Recovery = %#v, want successful recreation of %s", result, first.Name)
	}
	if _, err := clientset.CoreV1().Pods("shipyardhq").Get(context.Background(), first.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted pod lookup error = %v, want NotFound", err)
	}

	secondReport := recoveryReport(second, now.Add(-9*time.Minute))
	if err := Apply(context.Background(), client, profile, secondReport, Options{Enabled: true, StateDB: stateDB, Now: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	secondResult := secondReport.Workloads[0].Recovery
	if secondResult == nil || secondResult.Attempted {
		t.Fatalf("Recovery = %#v, want cooldown block", secondResult)
	}
	if !strings.Contains(secondResult.Reason, "cooldown") {
		t.Fatalf("Recovery reason = %q, want cooldown", secondResult.Reason)
	}
	if _, err := clientset.CoreV1().Pods("shipyardhq").Get(context.Background(), second.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("second pod should remain during cooldown: %v", err)
	}
}

func TestApplyRequiresCommandAndWorkloadOptIn(t *testing.T) {
	now := time.Date(2026, 7, 12, 14, 30, 0, 0, time.UTC)
	tests := []struct {
		name          string
		commandOptIn  bool
		workloadOptIn bool
		wantReason    string
	}{
		{name: "command disabled", workloadOptIn: true, wantReason: "pass --availability-recovery"},
		{name: "policy disabled", commandOptIn: true, wantReason: "disabled by workload policy"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := failedPod("shipyardhq-1", "uid-1", now.Add(-10*time.Minute))
			clientset := fake.NewSimpleClientset(pod)
			client := kube.NewFakeClient(clientset)
			profile := recoveryProfile()
			profile.Spec.Workloads[0].Policy.AvailabilityRecovery.Enabled = test.workloadOptIn
			report := recoveryReport(pod, now.Add(-10*time.Minute))

			if err := Apply(context.Background(), client, profile, report, Options{
				Enabled: test.commandOptIn,
				StateDB: filepath.Join(t.TempDir(), "state.db"),
				Now:     now,
			}); err != nil {
				t.Fatal(err)
			}
			result := report.Workloads[0].Recovery
			if result == nil || result.Attempted || !strings.Contains(result.Reason, test.wantReason) {
				t.Fatalf("Recovery = %#v, want opt-in block containing %q", result, test.wantReason)
			}
			if _, err := clientset.CoreV1().Pods("shipyardhq").Get(context.Background(), pod.Name, metav1.GetOptions{}); err != nil {
				t.Fatalf("pod changed without both opt-ins: %v", err)
			}
		})
	}
}

func TestApplyWaitsForFailureGracePeriod(t *testing.T) {
	now := time.Date(2026, 7, 12, 14, 30, 0, 0, time.UTC)
	pod := failedPod("shipyardhq-1", "uid-1", now.Add(-time.Minute))
	clientset := fake.NewSimpleClientset(pod)
	client := kube.NewFakeClient(clientset)
	report := recoveryReport(pod, now.Add(-time.Minute))

	if err := Apply(context.Background(), client, recoveryProfile(), report, Options{
		Enabled: true,
		StateDB: filepath.Join(t.TempDir(), "state.db"),
		Now:     now,
	}); err != nil {
		t.Fatal(err)
	}
	result := report.Workloads[0].Recovery
	if result == nil || result.Attempted || !strings.Contains(result.Reason, "grace period") {
		t.Fatalf("Recovery = %#v, want grace-period block", result)
	}
	if _, err := clientset.CoreV1().Pods("shipyardhq").Get(context.Background(), pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("pod changed during grace period: %v", err)
	}
}

func TestApplyDoesNotDeletePodThatRecoveredAfterAnalysis(t *testing.T) {
	now := time.Date(2026, 7, 12, 14, 30, 0, 0, time.UTC)
	pod := failedPod("shipyardhq-1", "uid-1", now.Add(-10*time.Minute))
	report := recoveryReport(pod, now.Add(-10*time.Minute))
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}}
	pod.Status.ContainerStatuses[0].Ready = true
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	clientset := fake.NewSimpleClientset(pod)
	client := kube.NewFakeClient(clientset)

	if err := Apply(context.Background(), client, recoveryProfile(), report, Options{
		Enabled: true,
		StateDB: filepath.Join(t.TempDir(), "state.db"),
		Now:     now,
	}); err != nil {
		t.Fatal(err)
	}
	result := report.Workloads[0].Recovery
	if result == nil || result.Attempted || !strings.Contains(result.Reason, "recovered after analysis") {
		t.Fatalf("Recovery = %#v, want recovered-pod skip", result)
	}
	if _, err := clientset.CoreV1().Pods("shipyardhq").Get(context.Background(), pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("recovered pod was changed: %v", err)
	}
}

func TestApplyRefusesPodWithoutDeploymentController(t *testing.T) {
	now := time.Date(2026, 7, 12, 14, 30, 0, 0, time.UTC)
	pod := failedPod("shipyardhq-1", "uid-1", now.Add(-10*time.Minute))
	report := recoveryReport(pod, now.Add(-10*time.Minute))
	pod.OwnerReferences = nil
	clientset := fake.NewSimpleClientset(pod)
	client := kube.NewFakeClient(clientset)

	if err := Apply(context.Background(), client, recoveryProfile(), report, Options{
		Enabled: true,
		StateDB: filepath.Join(t.TempDir(), "state.db"),
		Now:     now,
	}); err != nil {
		t.Fatal(err)
	}
	result := report.Workloads[0].Recovery
	if result == nil || result.Attempted || !strings.Contains(result.Reason, "not controlled by a Deployment ReplicaSet") {
		t.Fatalf("Recovery = %#v, want controller ownership block", result)
	}
	if _, err := clientset.CoreV1().Pods("shipyardhq").Get(context.Background(), pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("uncontrolled pod was changed: %v", err)
	}
}

func failedPod(name, uid string, failedAt time.Time) *corev1.Pod {
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "shipyardhq",
			UID:       types.UID(uid),
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "shipyardhq-rs", UID: "rs-uid", Controller: &controller},
			},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{
				Name: "web",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   133,
					Reason:     "Error",
					FinishedAt: metav1.NewTime(failedAt),
				}},
			},
		}},
	}
}

func recoveryProfile() *config.ApplicationProfile {
	return &config.ApplicationProfile{
		Spec: config.ApplicationSpec{Workloads: []config.WorkloadSpec{
			{
				Name: "web",
				Policy: config.PolicySpec{AvailabilityRecovery: config.AvailabilityRecoveryPolicySpec{
					Enabled:            true,
					FailureGracePeriod: "2m",
					Cooldown:           "5m",
					MaxAttemptsPerHour: 6,
				}},
			},
		}},
	}
}

func recoveryReport(pod *corev1.Pod, failedAt time.Time) *analyzer.Report {
	return &analyzer.Report{
		Application: "shipyard",
		Workloads: []analyzer.WorkloadReport{
			{
				Name:       "web",
				Namespace:  "shipyardhq",
				Deployment: "shipyardhq",
				Availability: analyzer.AvailabilityReport{
					Emergency: true,
					FailedPods: []analyzer.FailedPodReport{
						{Name: pod.Name, UID: string(pod.UID), Container: "web", Reason: "Error", ExitCode: 133, FailedAt: failedAt},
					},
				},
			},
		},
	}
}
