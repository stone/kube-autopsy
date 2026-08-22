// Package v1alpha1 contains API Schema definitions for the autopsy.tty.se v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=autopsy.tty.se
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label keys applied to every PodCrashReport, so reports can be selected
// server-side instead of by filtering the whole collection client-side.
const (
	LabelPod       = "autopsy.tty.se/pod"
	LabelContainer = "autopsy.tty.se/container"
	LabelNode      = "autopsy.tty.se/node"
	LabelReason    = "autopsy.tty.se/reason"
)

// Termination reasons.
const (
	TerminationOOMKilled = "OOMKilled"
	TerminationError     = "Error"
)

// Processing phases.
const (
	PhasePending   = "Pending"
	PhaseProcessed = "Processed"
)

// OOM scope classifications.
const (
	OOMContextContainerLimit = "ContainerLimit"
	OOMContextNodeExhaustion = "NodeExhaustion"
)

// MaxLogLines is the largest number of log lines a report may carry. It must
// stay in step with the MaxItems marker on DiagnosticData.LastLogLines below:
// the API server rejects an over-long list outright, which fails the status
// write and takes every other diagnostic down with the logs. A test asserts the
// two agree.
const MaxLogLines = 200

// PodCrashReportSpec defines the desired state of PodCrashReport.
type PodCrashReportSpec struct {
	// PodName is the name of the crashed pod.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	PodName string `json:"podName"`
	// PodUID identifies the specific pod incarnation that crashed. A pod name
	// is reused across restarts and recreations, so without this a report
	// cannot be attributed to the exact pod it came from.
	// +kubebuilder:validation:MaxLength=36
	PodUID string `json:"podUID,omitempty"`
	// Namespace is the namespace of the crashed pod.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// ContainerName is the specific container that crashed.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ContainerName string `json:"containerName"`
	// NodeName is the node where the pod was running.
	// +kubebuilder:validation:MaxLength=253
	NodeName string `json:"nodeName"`
	// TerminationReason is the reason for termination.
	// +kubebuilder:validation:Enum=OOMKilled;Error
	TerminationReason string `json:"terminationReason"`
	// ExitCode is the container's exit code.
	ExitCode int32 `json:"exitCode"`
	// Timestamp is the time the crash was detected.
	Timestamp metav1.Time `json:"timestamp"`
}

// PodCrashReportStatus defines the observed state of PodCrashReport.
type PodCrashReportStatus struct {
	// Diagnostics contains the captured diagnostic data.
	Diagnostics DiagnosticData `json:"diagnostics,omitempty"`
	// Phase indicates the processing state. It is empty until the agent has
	// attached diagnostics, which is how the controller knows not to process a
	// report that is still being written.
	// +kubebuilder:validation:Enum=Pending;Processed
	Phase string `json:"phase,omitempty"`
	// NotifiedAt records when a webhook notification was delivered, so a
	// delivery is not repeated once it has succeeded.
	NotifiedAt *metav1.Time `json:"notifiedAt,omitempty"`
}

// DiagnosticData contains captured system-level diagnostic information.
type DiagnosticData struct {
	// OOMScopeLimitBytes is the total memory available to the scope the OOM
	// killer was operating in: the container's memory limit for an OOM caused
	// by ContainerLimit, or the node's total RAM for NodeExhaustion. It is a
	// capacity, not a measure of what the victim was using — see
	// VictimRSSBytes for that.
	OOMScopeLimitBytes int64 `json:"oomScopeLimitBytes,omitempty"`
	// VictimRSSBytes is the victim's resident set size at the moment the OOM
	// killer selected it: anonymous plus file-backed plus shared memory, which
	// is what the kernel's own get_mm_rss() counts and what oom_badness() scores
	// the victim on. It is omitted when the running kernel's memory layout was
	// not recognised.
	VictimRSSBytes int64 `json:"victimRssBytes,omitempty"`
	// OOMVictimPID is the PID of the process killed by the OOM killer.
	OOMVictimPID int32 `json:"oomVictimPid,omitempty"`
	// OOMVictimComm is the process name of the OOM victim (e.g., "java", "node").
	OOMVictimComm string `json:"oomVictimComm,omitempty"`
	// TriggerPID is the PID of the process that triggered the OOM.
	TriggerPID int32 `json:"triggerPid,omitempty"`
	// TriggerComm is the name of the process that triggered the OOM.
	TriggerComm string `json:"triggerComm,omitempty"`
	// OOMScore is the kernel's "badness" score for the victim, in pages. This
	// is oom_control's chosen_points, not the 0-1000 value that
	// /proc/<pid>/oom_score reports.
	OOMScore int64 `json:"oomScore,omitempty"`
	// OOMScoreAdj is the adjustment score applied to the victim.
	OOMScoreAdj int32 `json:"oomScoreAdj,omitempty"`
	// RSSDissection contains the memory breakdown at the time of OOM. It is
	// omitted when the running kernel's memory layout was not recognised.
	RSSDissection *RSSDissection `json:"rssDissection,omitempty"`
	// OOMContext categorizes if the OOM was ContainerLimit or NodeExhaustion.
	// +kubebuilder:validation:Enum=ContainerLimit;NodeExhaustion
	OOMContext string `json:"oomContext,omitempty"`
	// LastLogLines are the final log lines captured before container
	// termination. Only populated when the agent runs with --capture-logs.
	//
	// The bounds are enforced by the API server as well as by the agent: a
	// report that grows past etcd's per-object limit cannot be written at all,
	// which would lose every other diagnostic alongside the logs.
	// +kubebuilder:validation:MaxItems=200
	// +kubebuilder:validation:items:MaxLength=4096
	LastLogLines []string `json:"lastLogLines,omitempty"`
}

// RSSDissection breaks down the Resident Set Size usage.
//
// AnonRSSBytes, FileRSSBytes and ShmemRSSBytes sum to VictimRSSBytes. Adding
// SwapBytes and PageTablesBytes gives the figure the kernel scored the victim
// on, which is reported separately as OOMScore.
type RSSDissection struct {
	AnonRSSBytes int64 `json:"anonRssBytes,omitempty"`
	FileRSSBytes int64 `json:"fileRssBytes,omitempty"`
	// ShmemRSSBytes is resident shared memory mapped into the victim: SysV and
	// POSIX shared memory, and mapped tmpfs such as /dev/shm. Workloads built
	// around shared memory — Postgres, and anything using a shared buffer pool —
	// look far smaller than they are without it.
	ShmemRSSBytes int64 `json:"shmemRssBytes,omitempty"`
	// SwapBytes is memory the victim had swapped out. It is not resident, so it
	// is not part of VictimRSSBytes, but the OOM killer counts it against the
	// victim.
	SwapBytes       int64 `json:"swapBytes,omitempty"`
	PageTablesBytes int64 `json:"pageTablesBytes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pcr
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.spec.podName`
// +kubebuilder:printcolumn:name="Container",type=string,JSONPath=`.spec.containerName`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.terminationReason`
// +kubebuilder:printcolumn:name="Exit",type=integer,JSONPath=`.spec.exitCode`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PodCrashReport is the Schema for the podcrashreports API.
type PodCrashReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodCrashReportSpec   `json:"spec,omitempty"`
	Status PodCrashReportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodCrashReportList contains a list of PodCrashReport.
type PodCrashReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodCrashReport `json:"items"`
}
