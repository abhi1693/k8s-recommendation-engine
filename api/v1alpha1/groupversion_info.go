// Package v1alpha1 contains API schema definitions for k8s-recommendation-engine.io/v1alpha1.
// +kubebuilder:object:generate=true
// +groupName=k8s-recommendation-engine.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion = schema.GroupVersion{Group: "k8s-recommendation-engine.io", Version: "v1alpha1"}

	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	AddToScheme = SchemeBuilder.AddToScheme
)
