package analyzer

import "k8s.io/apimachinery/pkg/api/resource"

func availabilityRecoveryChange(recommendation Recommendation) bool {
	if !recommendation.AvailabilityRecovery || !recommendationHasChange(recommendation) {
		return false
	}
	if recommendation.RecommendedReplicas < recommendation.CurrentReplicas {
		return false
	}
	if !resourceRequestDoesNotDecrease(recommendation.CurrentCPURequest, recommendation.RecommendedCPURequest) {
		return false
	}
	if !resourceRequestDoesNotDecrease(recommendation.CurrentMemoryRequest, recommendation.RecommendedMemoryRequest) {
		return false
	}
	return true
}

func resourceRequestDoesNotDecrease(current, recommended string) bool {
	if current == recommended {
		return true
	}
	if current == "" || recommended == "" {
		return false
	}
	currentQuantity, currentErr := resource.ParseQuantity(current)
	recommendedQuantity, recommendedErr := resource.ParseQuantity(recommended)
	return currentErr == nil && recommendedErr == nil && recommendedQuantity.Cmp(currentQuantity) >= 0
}
