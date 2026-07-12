package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

func TestDeletePodUsesUIDPrecondition(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shipyardhq-1", Namespace: "shipyardhq", UID: types.UID("uid-1"), ResourceVersion: "42"},
	})
	client := NewFakeClient(clientset)
	if err := client.DeletePod(context.Background(), "shipyardhq", "shipyardhq-1", "uid-1", "42"); err != nil {
		t.Fatal(err)
	}

	actions := clientset.Actions()
	if len(actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(actions))
	}
	deleteAction, ok := actions[0].(kubetesting.DeleteAction)
	if !ok {
		t.Fatalf("action = %T, want DeleteAction", actions[0])
	}
	options := deleteAction.GetDeleteOptions()
	wantUID := types.UID("uid-1")
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != wantUID {
		t.Fatalf("DeleteOptions = %#v, want UID precondition %q", options, wantUID)
	}
	if options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != "42" {
		t.Fatalf("DeleteOptions = %#v, want resourceVersion precondition 42", options)
	}
	if options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationOrphan {
		t.Fatalf("DeleteOptions = %#v, must not orphan pod dependents", options)
	}
}
