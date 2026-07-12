//go:build integration

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	recommendationv1alpha1 "github.com/abhi1693/k8s-recommendation-engine/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"
)

func TestControllerManagerReconcilesMultipleApplicationProfiles(t *testing.T) {
	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := testEnvironment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := recommendationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	processor := &fakeProcessor{report: healthyReport()}
	reconciler := &ApplicationProfileReconciler{
		Client:                   manager.GetClient(),
		Scheme:                   manager.GetScheme(),
		Processor:                processor,
		DefaultReconcileInterval: time.Minute,
		ReconcileTimeout:         30 * time.Second,
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	managerDone := make(chan error, 1)
	go func() {
		managerDone <- manager.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-managerDone:
			if err != nil {
				t.Errorf("manager stopped with error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})

	directClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	sample, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", "k8s-recommendation-engine_v1alpha1_applicationprofile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	template := &recommendationv1alpha1.ApplicationProfile{}
	if err := yaml.Unmarshal(sample, template); err != nil {
		t.Fatal(err)
	}
	var resources []*recommendationv1alpha1.ApplicationProfile
	for _, name := range []string{"example-a", "example-b"} {
		resource := template.DeepCopy()
		resource.Name = name
		resource.Namespace = "default"
		resource.Spec.Suspend = false
		resource.ResourceVersion = ""
		resource.UID = ""
		resource.CreationTimestamp = metav1.Time{}
		if err := directClient.Create(context.Background(), resource); err != nil {
			t.Fatal(err)
		}
		resources = append(resources, resource)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		readyProfiles := 0
		for _, resource := range resources {
			var observed recommendationv1alpha1.ApplicationProfile
			if err := directClient.Get(context.Background(), client.ObjectKeyFromObject(resource), &observed); err == nil {
				ready := conditionStatus(observed.Status.Conditions, ConditionReady)
				if ready == metav1.ConditionTrue && observed.Status.LastSuccessfulTime != nil {
					readyProfiles++
				}
			}
		}
		if readyProfiles == len(resources) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d ApplicationProfiles reconciled before timeout", readyProfiles, len(resources))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processor.calls.Load() < int32(len(resources)) {
		t.Fatalf("processor calls = %d, want at least %d", processor.calls.Load(), len(resources))
	}
}

func conditionStatus(conditions []metav1.Condition, conditionType string) metav1.ConditionStatus {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status
		}
	}
	return metav1.ConditionUnknown
}
