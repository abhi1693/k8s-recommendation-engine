package recovery

import (
	"context"
	"fmt"
	"time"

	"github.com/abhi1693/k8s-recommendation-engine/internal/analyzer"
	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
	"github.com/abhi1693/k8s-recommendation-engine/internal/kube"
	"github.com/abhi1693/k8s-recommendation-engine/internal/state"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultFailureGracePeriod = 2 * time.Minute
	defaultRecoveryCooldown   = 5 * time.Minute
	defaultMaxAttemptsPerHour = 6
)

type Options struct {
	Enabled bool
	StateDB string
	Now     time.Time
}

func Apply(ctx context.Context, client *kube.Client, profile *config.ApplicationProfile, report *analyzer.Report, options Options) error {
	if client == nil || profile == nil || report == nil {
		return nil
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	workloads := make(map[string]config.WorkloadSpec, len(profile.Spec.Workloads))
	for _, workload := range profile.Spec.Workloads {
		workloads[workload.Name] = workload
	}

	for index := range report.Workloads {
		workload := &report.Workloads[index]
		if !workload.Availability.Emergency {
			continue
		}
		spec, ok := workloads[workload.Name]
		if !ok {
			continue
		}
		result := &analyzer.AvailabilityRecoveryReport{
			Enabled:   options.Enabled && spec.Policy.AvailabilityRecovery.Enabled,
			Emergency: true,
			Action:    "recreate_failed_pod",
		}
		workload.Recovery = result
		if !options.Enabled {
			result.Reason = "live availability recovery is disabled; pass --availability-recovery to enable"
			continue
		}
		if !spec.Policy.AvailabilityRecovery.Enabled {
			result.Reason = "live availability recovery is disabled by workload policy"
			continue
		}

		gracePeriod := policyDuration(spec.Policy.AvailabilityRecovery.FailureGracePeriod, defaultFailureGracePeriod)
		candidate, ok := recoveryCandidate(workload.Availability.FailedPods, now, gracePeriod)
		if !ok {
			result.Reason = fmt.Sprintf("failed pods have not exceeded recovery grace period %s", gracePeriod)
			continue
		}
		result.Pod = candidate.Name
		livePod, err := client.GetPod(ctx, workload.Namespace, candidate.Name)
		if apierrors.IsNotFound(err) {
			result.Reason = "failed pod no longer exists; its controller may already be recreating it"
			continue
		}
		if err != nil {
			result.Error = err.Error()
			result.Reason = "failed to recheck pod before availability recovery"
			continue
		}
		if candidate.UID != "" && string(livePod.UID) != candidate.UID {
			result.Reason = "pod was replaced after analysis; waiting for the replacement status"
			continue
		}
		controller := metav1.GetControllerOf(livePod)
		if controller == nil || controller.APIVersion != "apps/v1" || controller.Kind != "ReplicaSet" {
			result.Reason = "failed pod is not controlled by a Deployment ReplicaSet; refusing direct recreation"
			continue
		}
		failedAt, stillFailed := currentPodFailure(livePod, candidate.Container)
		if !stillFailed {
			result.Reason = "pod recovered after analysis; no recreation is needed"
			continue
		}
		if failedAt.IsZero() || now.Sub(failedAt) < gracePeriod {
			result.Reason = fmt.Sprintf("current pod failure has not exceeded recovery grace period %s", gracePeriod)
			continue
		}
		candidate.UID = string(livePod.UID)
		candidate.FailedAt = failedAt
		cooldown := policyDuration(spec.Policy.AvailabilityRecovery.Cooldown, defaultRecoveryCooldown)
		maxAttempts := spec.Policy.AvailabilityRecovery.MaxAttemptsPerHour
		if maxAttempts <= 0 {
			maxAttempts = defaultMaxAttemptsPerHour
		}
		reservation, err := state.ReserveAvailabilityRecovery(ctx, options.StateDB, state.AvailabilityRecoveryKey{
			Application: report.Application,
			Namespace:   workload.Namespace,
			Workload:    workload.Name,
			Deployment:  workload.Deployment,
			Pod:         candidate.Name,
			PodUID:      candidate.UID,
		}, now, cooldown, maxAttempts)
		if err != nil {
			return err
		}
		if !reservation.Allowed {
			result.Reason = reservation.Reason
			continue
		}

		result.Attempted = true
		if err := client.DeletePod(ctx, workload.Namespace, candidate.Name, candidate.UID, livePod.ResourceVersion); err != nil {
			result.Error = err.Error()
			result.Reason = "failed pod recreation request was rejected"
			if recordErr := state.CompleteAvailabilityRecovery(ctx, options.StateDB, reservation.ID, "failed", err.Error()); recordErr != nil {
				return recordErr
			}
			continue
		}
		result.Succeeded = true
		result.Reason = fmt.Sprintf("deleted failed pod after %s; its controller will recreate it", now.Sub(candidate.FailedAt).Round(time.Second))
		if err := state.CompleteAvailabilityRecovery(ctx, options.StateDB, reservation.ID, "succeeded", result.Reason); err != nil {
			return err
		}
	}
	return nil
}

func currentPodFailure(pod *corev1.Pod, containerName string) (time.Time, bool) {
	if pod == nil || pod.DeletionTimestamp != nil || podReady(pod) {
		return time.Time{}, false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != containerName || status.Ready {
			continue
		}
		terminated := status.State.Terminated
		if terminated == nil && status.State.Waiting != nil && status.State.Waiting.Reason == "CrashLoopBackOff" {
			terminated = status.LastTerminationState.Terminated
		}
		if terminated == nil || terminated.ExitCode == 0 {
			return time.Time{}, false
		}
		return terminated.FinishedAt.Time, true
	}
	return time.Time{}, false
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func recoveryCandidate(pods []analyzer.FailedPodReport, now time.Time, gracePeriod time.Duration) (analyzer.FailedPodReport, bool) {
	for _, pod := range pods {
		if pod.FailedAt.IsZero() || now.Sub(pod.FailedAt) < gracePeriod {
			continue
		}
		return pod, true
	}
	return analyzer.FailedPodReport{}, false
}

func policyDuration(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}
