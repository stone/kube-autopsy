package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

func TestDeleteExpiredReports(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add to scheme: %v", err)
	}

	now := time.Now()
	ttl := 24 * time.Hour

	// Create test reports
	reportExpired := &v1alpha1.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "expired-report",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: now.Add(-48 * time.Hour)}, // 48 hours old
		},
	}

	reportFresh := &v1alpha1.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "fresh-report",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: now.Add(-1 * time.Hour)}, // 1 hour old
		},
	}

	reportBorderline := &v1alpha1.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "borderline-report",
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: now.Add(-23 * time.Hour)}, // 23 hours old
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(reportExpired, reportFresh, reportBorderline).
		Build()

	gc := NewGarbageCollector(fakeClient, ttl)

	// Inject the metrics registerer to prevent panic if tests run in parallel
	// though they are globally registered in metrics.go, no setup needed for this basic test.
	
	deleted, err := gc.deleteExpiredReports(context.Background())
	if err != nil {
		t.Fatalf("deleteExpiredReports failed: %v", err)
	}

	// We expect only 1 report (expired-report) to be deleted
	if deleted != 1 {
		t.Errorf("expected 1 deleted report, got %d", deleted)
	}

	// Verify only the expired report was deleted from the fake client
	var remainingReports v1alpha1.PodCrashReportList
	if err := fakeClient.List(context.Background(), &remainingReports); err != nil {
		t.Fatalf("failed to list remaining reports: %v", err)
	}

	if len(remainingReports.Items) != 2 {
		t.Errorf("expected 2 remaining reports, got %d", len(remainingReports.Items))
	}

	for _, item := range remainingReports.Items {
		if item.Name == "expired-report" {
			t.Errorf("expired report was not deleted from the cluster")
		}
	}
}
