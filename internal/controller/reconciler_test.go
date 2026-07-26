package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
	"github.com/kube-autopsy/kube-autopsy/internal/config"
)

func TestReconcileEmitsEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add to scheme: %v", err)
	}

	report := &v1alpha1.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-crash-report",
			Namespace: "default",
		},
		Spec: v1alpha1.PodCrashReportSpec{
			PodName:       "test-pod",
			Namespace:     "default",
			ContainerName: "test-container",
			Termination:   "OOMKilled",
			ExitCode:      137,
		},
		Status: v1alpha1.PodCrashReportStatus{
			Phase: "Pending",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(report).
		WithStatusSubresource(report).
		Build()

	fakeRecorder := record.NewFakeRecorder(10)
	cfg := config.NewConfig()

	reconciler := &PodCrashReportReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Config:   cfg,
		Recorder: fakeRecorder,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-crash-report",
			Namespace: "default",
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	select {
	case event := <-fakeRecorder.Events:
		expectedSubstr := "Warning CrashDetected Processed crash report for pod default/test-pod"
		if len(event) < len(expectedSubstr) || event[:len(expectedSubstr)] != expectedSubstr {
			t.Errorf("expected event starting with %q, got %q", expectedSubstr, event)
		}
	default:
		t.Errorf("expected an event to be emitted on the fake recorder, but none was received")
	}
}
