package agent

import (
	"context"
	"fmt"
	"strings"

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
	client client.Client
	// nodeName is the node this agent runs on.
	nodeName string
	// podOwnerReference makes each report owned by the pod it describes, so
	// Kubernetes deletes it along with the pod. Off by default: a post-mortem
	// that disappears when the pod is recycled is usually gone before anyone
	// reads it. Retention is otherwise handled by the controller's TTL.
	podOwnerReference bool
}

// NewReporter creates a new Reporter that uses the given Kubernetes client
// and node name for creating crash reports.
func NewReporter(client client.Client, nodeName string, podOwnerReference bool) *Reporter {
	return &Reporter{
		client:            client,
		nodeName:          nodeName,
		podOwnerReference: podOwnerReference,
	}
}

// CreateCrashReport creates a PodCrashReport custom resource in Kubernetes.
// The name is server-generated from the prefix <podName>-<containerName>-, so
// a container that is OOM-killed repeatedly produces one report per kill rather
// than colliding on a deterministic name. Owner references are set to the
// originating pod when possible.
func (r *Reporter) CreateCrashReport(ctx context.Context, event CrashEvent, podMeta PodMeta, logLines []string) error {
	// Calculate OOMContext
	oomContext := autopsy.OOMContextContainerLimit
	if event.IsGlobalOOM {
		oomContext = autopsy.OOMContextNodeExhaustion
	}

	report := &autopsy.PodCrashReport{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: reportNamePrefix(podMeta.PodName, podMeta.ContainerName),
			Namespace:    podMeta.Namespace,
			// Names are server-generated and therefore opaque, so these labels
			// are how a report is found: they allow server-side selection with
			// `kubectl get pcr -l ...` rather than filtering every object
			// client-side.
			Labels: map[string]string{
				autopsy.LabelPod:       labelValue(podMeta.PodName),
				autopsy.LabelContainer: labelValue(podMeta.ContainerName),
				autopsy.LabelNode:      labelValue(r.nodeName),
				autopsy.LabelReason:    autopsy.TerminationOOMKilled,
			},
		},
		Spec: autopsy.PodCrashReportSpec{
			PodName:           podMeta.PodName,
			PodUID:            podMeta.PodUID,
			Namespace:         podMeta.Namespace,
			ContainerName:     podMeta.ContainerName,
			NodeName:          r.nodeName,
			TerminationReason: autopsy.TerminationOOMKilled,
			ExitCode:          137, // Standard OOM kill exit code (128 + SIGKILL=9).
			Timestamp:         metav1.NewTime(event.DetectedAt.UTC()),
		},
	}

	if r.podOwnerReference {
		setOwnerReference(report, podMeta)
	}

	if err := r.client.Create(ctx, report); err != nil {
		return fmt.Errorf("failed to create PodCrashReport in %s: %w", podMeta.Namespace, err)
	}

	// Because 'status' is a subresource, Create() ignores the Status field, so
	// the diagnostics need a second write. The patch carries an optimistic-lock
	// precondition on the resourceVersion returned by Create: if anything else
	// has written to the report in the meantime, fail loudly rather than
	// silently clobbering it.
	patch := client.MergeFromWithOptions(report.DeepCopy(), client.MergeFromWithOptimisticLock{})

	diagnostics := autopsy.DiagnosticData{
		OOMScopeLimitBytes: event.OOMScopeLimitBytes,
		OOMVictimPID:       event.OOMVictimPID,
		OOMVictimComm:      event.OOMVictimComm,
		TriggerPID:         event.TriggerPID,
		TriggerComm:        event.TriggerComm,
		OOMScore:           event.OOMScore,
		OOMScoreAdj:        event.OOMScoreAdj,
		OOMContext:         oomContext,
		LastLogLines:       logLines,
	}

	// Only publish memory figures the kernel actually gave us. Reporting zeroes
	// would be indistinguishable from a container that used no memory.
	if event.RSSValid {
		diagnostics.VictimRSSBytes = event.VictimRSSBytes
		diagnostics.RSSDissection = &autopsy.RSSDissection{
			AnonRSSBytes:    event.AnonRSSBytes,
			FileRSSBytes:    event.FileRSSBytes,
			PageTablesBytes: event.PageTablesBytes,
		}
	}

	report.Status = autopsy.PodCrashReportStatus{
		Diagnostics: diagnostics,
		Phase:       "Pending",
	}

	if err := r.client.Status().Patch(ctx, report, patch); err != nil {
		return fmt.Errorf("failed to update PodCrashReport status %s/%s: %w", podMeta.Namespace, report.Name, err)
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

// setOwnerReference sets the pod as the owner of the PodCrashReport, so the
// report is garbage-collected when the pod is deleted. The pod identity comes
// from the metadata already resolved by ResolvePodMeta — re-reading it here
// would cost an extra full-namespace List per crash event.
func setOwnerReference(report *autopsy.PodCrashReport, podMeta PodMeta) {
	if podMeta.PodName == "" || podMeta.PodUID == "" {
		return
	}

	isController := false
	// Deliberately false: blockOwnerDeletion requires "update" on
	// pods/finalizers wherever the OwnerReferencesPermissionEnforcement
	// admission plugin is enabled (OpenShift), which the agent is not granted.
	blockOwnerDeletion := false
	report.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion:         "v1",
			Kind:               "Pod",
			Name:               podMeta.PodName,
			UID:                types.UID(podMeta.PodUID),
			Controller:         &isController,
			BlockOwnerDeletion: &blockOwnerDeletion,
		},
	}
}

// findContainerByID inspects a pod's container statuses to find one that matches
// the given container ID.
func findContainerByID(pod corev1.Pod, containerID string) string {
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses)+len(pod.Status.EphemeralContainerStatuses))
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.EphemeralContainerStatuses...)

	for _, cs := range statuses {
		if containerIDsMatch(cs.ContainerID, containerID) {
			return cs.Name
		}
	}
	return ""
}

// containerIDsMatch compares a ContainerStatus ID such as
// "containerd://<64-hex>" with an ID derived from a cgroup name. The runtime
// scheme is stripped and the remainder compared exactly: a substring match
// could attribute a crash to the wrong container whenever one ID happens to
// contain another, and would also match against the scheme itself.
func containerIDsMatch(statusID, cgroupID string) bool {
	if statusID == "" || cgroupID == "" {
		return false
	}
	if _, id, found := strings.Cut(statusID, "://"); found {
		statusID = id
	}
	return statusID == cgroupID
}

// generatedNameBaseLimit is the longest GenerateName prefix the API server
// preserves: it truncates the base to 63 characters less the 5-character
// random suffix it appends.
const generatedNameBaseLimit = 63 - 5

// reportNamePrefix builds the GenerateName prefix for a crash report, trimmed
// so the API server does not truncate it into something unrecognisable.
func reportNamePrefix(podName, containerName string) string {
	base := sanitizeName(fmt.Sprintf("%s-%s", podName, containerName))
	if base == "" {
		base = "podcrashreport"
	}
	if len(base) > generatedNameBaseLimit-1 {
		base = base[:generatedNameBaseLimit-1]
	}
	return strings.TrimRight(base, "-.") + "-"
}

// maxLabelValueLength is the Kubernetes limit for a label value.
const maxLabelValueLength = 63

// labelValue coerces s into a valid Kubernetes label value: at most 63
// characters of alphanumerics, '-', '_' or '.', beginning and ending with an
// alphanumeric. An invalid label would make the whole report rejected at
// creation, so this never returns something the API server will refuse.
func labelValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'),
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	out := b.String()
	if len(out) > maxLabelValueLength {
		out = out[:maxLabelValueLength]
	}
	// Leading and trailing non-alphanumerics are not permitted.
	return strings.Trim(out, "-_.")
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
