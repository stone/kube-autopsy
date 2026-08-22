package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
)

func reportAged(name string, age time.Duration) *v1alpha1.PodCrashReport {
	return &v1alpha1.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
		},
	}
}

func gcScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add to scheme: %v", err)
	}
	return scheme
}

// Retention was time-based only, which bounds how long a report lives but not
// how many exist at once — so a cluster-wide crash loop could grow the
// collection until the controller could no longer hold it.
func TestGarbageCollectorEnforcesReportCap(t *testing.T) {
	objs := []client.Object{
		reportAged("oldest", 5*time.Hour),
		reportAged("older", 4*time.Hour),
		reportAged("newer", 3*time.Hour),
		reportAged("newest", 1*time.Hour),
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(gcScheme(t)).
		WithObjects(objs...).
		Build()

	// A TTL far beyond every report, so only the cap can delete anything.
	gc := NewGarbageCollector(fakeClient, 30*24*time.Hour, 2)

	deleted, err := gc.deleteExpiredReports(context.Background())
	if err != nil {
		t.Fatalf("deleteExpiredReports: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d reports, want 2", deleted)
	}

	var remaining v1alpha1.PodCrashReportList
	if err := fakeClient.List(context.Background(), &remaining); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining.Items) != 2 {
		t.Fatalf("%d reports remain, want 2", len(remaining.Items))
	}

	// The oldest go first: a post-mortem loses value with age, and the newest
	// crash is the one someone is looking at.
	kept := map[string]bool{}
	for _, r := range remaining.Items {
		kept[r.Name] = true
	}
	for _, name := range []string{"newer", "newest"} {
		if !kept[name] {
			t.Errorf("expected %q to be kept", name)
		}
	}
}

func TestGarbageCollectorCapOfZeroIsDisabled(t *testing.T) {
	fakeClient := fake.NewClientBuilder().
		WithScheme(gcScheme(t)).
		WithObjects(reportAged("a", time.Hour), reportAged("b", time.Hour)).
		Build()

	gc := NewGarbageCollector(fakeClient, 30*24*time.Hour, 0)

	deleted, err := gc.deleteExpiredReports(context.Background())
	if err != nil {
		t.Fatalf("deleteExpiredReports: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted %d reports with the cap disabled, want 0", deleted)
	}
}

// A report exactly at the TTL is not yet expired; one a moment past it is.
func TestGarbageCollectorTTLBoundary(t *testing.T) {
	const ttl = 24 * time.Hour

	fakeClient := fake.NewClientBuilder().
		WithScheme(gcScheme(t)).
		WithObjects(
			reportAged("just-inside", ttl-time.Minute),
			reportAged("just-outside", ttl+time.Minute),
		).
		Build()

	gc := NewGarbageCollector(fakeClient, ttl, 0)

	if _, err := gc.deleteExpiredReports(context.Background()); err != nil {
		t.Fatalf("deleteExpiredReports: %v", err)
	}

	var remaining v1alpha1.PodCrashReportList
	if err := fakeClient.List(context.Background(), &remaining); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining.Items) != 1 || remaining.Items[0].Name != "just-inside" {
		t.Errorf("remaining = %v, want only just-inside", remaining.Items)
	}
}

// One undeletable report — a stuck finalizer, a revoked RBAC rule — must not
// stop the collector working through the rest.
func TestGarbageCollectorContinuesPastAFailedDelete(t *testing.T) {
	fakeClient := fake.NewClientBuilder().
		WithScheme(gcScheme(t)).
		WithObjects(
			reportAged("stuck", 48*time.Hour),
			reportAged("deletable-a", 48*time.Hour),
			reportAged("deletable-b", 48*time.Hour),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == "stuck" {
					return errors.New("simulated finalizer deadlock")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	gc := NewGarbageCollector(fakeClient, 24*time.Hour, 0)

	deleted, err := gc.deleteExpiredReports(context.Background())
	if err == nil {
		t.Error("expected the failure to be reported to the caller")
	}
	if deleted != 2 {
		t.Errorf("deleted %d reports, want the other 2 to still be collected", deleted)
	}

	var remaining v1alpha1.PodCrashReportList
	if err := fakeClient.List(context.Background(), &remaining); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining.Items) != 1 || remaining.Items[0].Name != "stuck" {
		t.Errorf("remaining = %v, want only the undeletable report", remaining.Items)
	}
}

// ReportAgeSeconds used to be observed before the delete, so a report that could
// never be deleted was re-observed with a larger age on every pass, forever —
// until the histogram described that one report rather than the fleet.
func TestReportAgeIsObservedOnlyOnSuccessfulDeletion(t *testing.T) {
	fakeClient := fake.NewClientBuilder().
		WithScheme(gcScheme(t)).
		WithObjects(reportAged("stuck", 48*time.Hour)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				return errors.New("simulated finalizer deadlock")
			},
		}).
		Build()

	gc := NewGarbageCollector(fakeClient, 24*time.Hour, 0)

	before := histogramCount(t, ReportAgeSeconds)
	for i := 0; i < 3; i++ {
		if _, err := gc.deleteExpiredReports(context.Background()); err == nil {
			t.Fatal("expected the delete to fail")
		}
	}
	after := histogramCount(t, ReportAgeSeconds)

	if after != before {
		t.Errorf("ReportAgeSeconds gained %d observations from deletions that never happened", after-before)
	}
}

// histogramCount reads the observation count out of a Prometheus histogram.
func histogramCount(t *testing.T, h interface{ Write(*dto.Metric) error }) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatalf("reading histogram: %v", err)
	}
	if m.Histogram == nil {
		t.Fatal("metric is not a histogram")
	}
	return m.Histogram.GetSampleCount()
}

// The cap applies to what survives the TTL. Testing it against the whole list
// labelled ordinary expiry as a trim whenever the collection also happened to be
// over the cap, so kube_autopsy_reports_trimmed_total reported pressure that was
// not there.
func TestGarbageCollectorDoesNotLabelExpiryAsTrimming(t *testing.T) {
	fakeClient := gcClientWith(t,
		// Both well past a 1h TTL.
		reportAged("expired-a", 48*time.Hour),
		reportAged("expired-b", 47*time.Hour),
		// Inside the TTL, and within a cap of 2.
		reportAged("live-a", 10*time.Minute),
		reportAged("live-b", 5*time.Minute),
	)

	gc := NewGarbageCollector(fakeClient, time.Hour, 2)

	before := counterValue(t, ReportsTrimmedTotal)
	deleted, err := gc.deleteExpiredReports(context.Background())
	if err != nil {
		t.Fatalf("deleteExpiredReports: %v", err)
	}
	after := counterValue(t, ReportsTrimmedTotal)

	if deleted != 2 {
		t.Errorf("deleted %d, want the 2 expired reports", deleted)
	}
	if after != before {
		t.Errorf("ReportsTrimmedTotal rose by %v; expiry must not be counted as trimming", after-before)
	}

	var remaining v1alpha1.PodCrashReportList
	if err := fakeClient.List(context.Background(), &remaining); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining.Items) != 2 {
		t.Errorf("%d reports remain, want the 2 live ones", len(remaining.Items))
	}
}

// A report that another actor deleted between the list and the delete is the
// outcome this pass wanted, not a failure to alert on.
func TestGarbageCollectorTreatsNotFoundAsDone(t *testing.T) {
	fakeClient := fake.NewClientBuilder().
		WithScheme(gcScheme(t)).
		WithObjects(reportAged("racing", 48*time.Hour)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				return apierrors.NewNotFound(
					schema.GroupResource{Group: "autopsy.tty.se", Resource: "podcrashreports"},
					obj.GetName())
			},
		}).
		Build()

	gc := NewGarbageCollector(fakeClient, 24*time.Hour, 0)

	before := counterValue(t, GCErrorsTotal)
	_, err := gc.deleteExpiredReports(context.Background())
	after := counterValue(t, GCErrorsTotal)

	if err != nil {
		t.Errorf("a NotFound deletion was reported as an error: %v", err)
	}
	if after != before {
		t.Error("GCErrorsTotal rose for a report that was already gone")
	}
}

func gcClientWith(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(gcScheme(t)).WithObjects(objs...).Build()
}

// counterValue reads the current value of a Prometheus counter.
func counterValue(t *testing.T, c prometheus.Metric) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("reading counter: %v", err)
	}
	if m.Counter == nil {
		t.Fatal("metric is not a counter")
	}
	return m.Counter.GetValue()
}
