package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	autopsy "github.com/kube-autopsy/kube-autopsy/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodMeta contains the Kubernetes metadata for a pod, resolved from its UID.
type PodMeta struct {
	// PodName is the name of the pod.
	PodName string
	// Namespace is the namespace the pod belongs to.
	Namespace string
	// ContainerName is the name of the container that crashed.
	ContainerName string
	// PodUID is the UID of the pod.
	PodUID string
}

// Reporter creates PodCrashReport CRD instances in the Kubernetes API.
type Reporter struct {
	client   client.Client
	nodeName string
}

// NewReporter creates a new Reporter that uses the given Kubernetes client
// and node name for creating crash reports.
func NewReporter(client client.Client, nodeName string) *Reporter {
	return &Reporter{
		client:   client,
		nodeName: nodeName,
	}
}

// CreateCrashReport creates a PodCrashReport custom resource in Kubernetes.
// The report name follows the pattern <podName>-<containerName>-<shortTimestamp>.
// Owner references are set to the originating pod when possible.
func (r *Reporter) CreateCrashReport(ctx context.Context, event CrashEvent, podMeta PodMeta, logLines []string) error {
	shortTimestamp := event.DetectedAt.UTC().Format("20060102-150405")
	reportName := sanitizeName(fmt.Sprintf("%s-%s-%s", podMeta.PodName, podMeta.ContainerName, shortTimestamp))

	// Kubernetes names must be <= 253 characters.
	if len(reportName) > 253 {
		reportName = reportName[:253]
	}

	// Calculate OOMContext
	oomContext := "ContainerLimit"
	if event.IsGlobalOOM {
		oomContext = "NodeExhaustion"
	}

	report := &autopsy.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      reportName,
			Namespace: podMeta.Namespace,
		},
		Spec: autopsy.PodCrashReportSpec{
			PodName:       podMeta.PodName,
			Namespace:     podMeta.Namespace,
			ContainerName: podMeta.ContainerName,
			NodeName:      r.nodeName,
			Termination:   "OOMKilled",
			ExitCode:      137, // Standard OOM kill exit code (128 + SIGKILL=9).
			Timestamp:     event.DetectedAt.UTC().Format(time.RFC3339),
		},
	}

	// Set owner reference to the pod if possible.
	if err := r.setOwnerReference(ctx, podMeta.PodUID, podMeta.Namespace, report); err != nil {
		log.V(1).Info("could not set owner reference, continuing without it",
			"podUID", podMeta.PodUID, "error", err)
	}

	if err := r.client.Create(ctx, report); err != nil {
		return fmt.Errorf("failed to create PodCrashReport %s/%s: %w", podMeta.Namespace, reportName, err)
	}

	// Because 'status' is a subresource, Create() ignores the Status field.
	// We must explicitly update the status subresource after creation using a Patch
	// to avoid ResourceVersion conflicts with the controller.
	patch := client.MergeFrom(report.DeepCopy())
	rssDissect := &autopsy.RSSDissection{
		AnonRSSBytes:    event.AnonRSSBytes,
		FileRSSBytes:    event.FileRSSBytes,
		PageTablesBytes: event.PageTablesBytes,
	}

	report.Status = autopsy.PodCrashReportStatus{
		Diagnostics: autopsy.DiagnosticData{
			PeakMemoryBytes: event.PeakMemoryBytes,
			OOMVictimPID:    event.OOMVictimPID,
			OOMVictimComm:   event.OOMVictimComm,
			TriggerPID:      event.TriggerPID,
			TriggerComm:     event.TriggerComm,
			OOMScore:        event.OOMScore,
			OOMScoreAdj:     event.OOMScoreAdj,
			RSSDissection:   rssDissect,
			OOMContext:      oomContext,
			LastLogLines:    logLines,
		},
		Phase: "Pending",
	}

	if err := r.client.Status().Patch(ctx, report, patch); err != nil {
		return fmt.Errorf("failed to update PodCrashReport status %s/%s: %w", podMeta.Namespace, reportName, err)
	}

	return nil
}

// ResolvePodMeta uses the Kubernetes API to find the pod matching the given event
// and returns its metadata including the name of the container that matches the
// crash event's container ID.
func (r *Reporter) ResolvePodMeta(ctx context.Context, event CrashEvent) (PodMeta, error) {
	// List all pods on this node to find the matching UID.
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList, client.MatchingFields{
		"spec.nodeName": r.nodeName,
	}); err != nil {
		return PodMeta{}, fmt.Errorf("failed to list pods on node %s: %w", r.nodeName, err)
	}

	for _, pod := range podList.Items {
		containerName := findContainerByID(pod, event.ContainerID)
		if containerName != "" {
			// Found the pod containing this container!
			// We populate PodUID so the rest of the flow can use it
			return PodMeta{
				PodName:       pod.Name,
				Namespace:     pod.Namespace,
				ContainerName: containerName,
				PodUID:        string(pod.UID),
			}, nil
		}
	}

	return PodMeta{}, fmt.Errorf("pod with container ID %s not found on node %s", event.ContainerID, r.nodeName)
}

// setOwnerReference sets the pod as the owner of the PodCrashReport.
// This ensures the report is garbage-collected when the pod is deleted.
func (r *Reporter) setOwnerReference(ctx context.Context, podUID, namespace string, report *autopsy.PodCrashReport) error {
	var pod corev1.Pod
	// We need to get the pod to set proper owner reference with the correct name.
	var podList corev1.PodList
	if err := r.client.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		return err
	}

	for _, p := range podList.Items {
		if string(p.UID) == podUID {
			pod = p
			isController := false
			blockOwnerDeletion := true
			report.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Pod",
					Name:               pod.Name,
					UID:                types.UID(podUID),
					Controller:         &isController,
					BlockOwnerDeletion: &blockOwnerDeletion,
				},
			}
			return nil
		}
	}

	return fmt.Errorf("pod with UID %s not found for owner reference", podUID)
}

// findContainerByID inspects a pod's container statuses to find one that matches
// the given container ID.
func findContainerByID(pod corev1.Pod, containerID string) string {
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses)+len(pod.Status.EphemeralContainerStatuses))
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.EphemeralContainerStatuses...)

	for _, cs := range statuses {
		if strings.Contains(cs.ContainerID, containerID) {
			return cs.Name
		}
	}
	return ""
}

// sanitizeName converts a string into a valid Kubernetes resource name by
// lowercasing and replacing invalid characters with dashes.
func sanitizeName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	result := b.String()
	// Trim leading/trailing dashes.
	result = strings.Trim(result, "-.")
	return result
}
