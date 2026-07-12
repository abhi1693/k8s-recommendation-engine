package controller

import (
	"encoding/json"
	"fmt"

	recommendationv1alpha1 "github.com/abhi1693/k8s-recommendation-engine/api/v1alpha1"
	"github.com/abhi1693/k8s-recommendation-engine/internal/config"
)

func profileConfig(resource *recommendationv1alpha1.ApplicationProfile) (*config.ApplicationProfile, error) {
	if resource == nil {
		return nil, fmt.Errorf("application profile is required")
	}
	raw, err := json.Marshal(resource.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal application profile spec: %w", err)
	}
	var spec config.ApplicationSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("convert application profile spec: %w", err)
	}
	var metrics struct {
		MetricProfiles map[string]config.MetricProfile `json:"metricProfiles"`
	}
	if err := json.Unmarshal(raw, &metrics); err != nil {
		return nil, fmt.Errorf("convert application metric profiles: %w", err)
	}
	identity := resource.Name
	if resource.Namespace != "" {
		identity = resource.Namespace + "/" + resource.Name
	}
	if resource.Spec.StateKey != "" {
		identity = resource.Spec.StateKey
	}

	profile := &config.ApplicationProfile{
		APIVersion: recommendationv1alpha1.GroupVersion.String(),
		Kind:       "ApplicationProfile",
		Metadata: config.Metadata{
			Name: identity,
		},
		Spec:           spec,
		MetricProfiles: metrics.MetricProfiles,
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return profile, nil
}
