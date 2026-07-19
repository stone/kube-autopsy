// Package v1alpha1 contains API Schema definitions for the autopsy.io v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=autopsy.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodCrashReportSpec defines the desired state of PodCrashReport.
type PodCrashReportSpec struct {
	// PodName is the name of the crashed pod.
	PodName string `json:"podName"`
	// Namespace is the namespace of the crashed pod.
	Namespace string `json:"namespace"`
	// ContainerName is the specific container that crashed.
	ContainerName string `json:"containerName"`
	// NodeName is the node where the pod was running.
	NodeName string `json:"nodeName"`
	// Termination is the reason for termination (e.g., OOMKilled, Error).
	Termination string `json:"terminationReason"`
	// ExitCode is the container's exit code.
	ExitCode int32 `json:"exitCode"`
	// Timestamp is the ISO 8601 time of the crash event.
	Timestamp string `json:"timestamp"`
}

// PodCrashReportStatus defines the observed state of PodCrashReport.
type PodCrashReportStatus struct {
	// Diagnostics contains the captured diagnostic data.
	Diagnostics DiagnosticData `json:"diagnostics,omitempty"`
	// Phase indicates the processing state: Pending or Processed.
	Phase string `json:"phase,omitempty"`
}

// DiagnosticData contains captured system-level diagnostic information.
type DiagnosticData struct {
	// PeakMemoryBytes is the peak memory usage from cgroup memory.peak.
	PeakMemoryBytes int64 `json:"peakMemoryBytes,omitempty"`
	// OOMVictimPID is the PID of the process killed by the OOM killer.
	OOMVictimPID int32 `json:"oomVictimPid,omitempty"`
	// OOMVictimComm is the process name of the OOM victim (e.g., "java", "node").
	OOMVictimComm string `json:"oomVictimComm,omitempty"`
	// TriggerPID is the PID of the process that triggered the OOM.
	TriggerPID int32 `json:"triggerPid,omitempty"`
	// TriggerComm is the name of the process that triggered the OOM.
	TriggerComm string `json:"triggerComm,omitempty"`
	// OOMScore is the kernel's calculated OOM score.
	OOMScore int32 `json:"oomScore,omitempty"`
	// OOMScoreAdj is the adjustment score applied to the victim.
	OOMScoreAdj int32 `json:"oomScoreAdj,omitempty"`
	// RSSDissection contains the memory breakdown at the time of OOM.
	RSSDissection *RSSDissection `json:"rssDissection,omitempty"`
	// OOMContext categorizes if the OOM was ContainerLimit or NodeExhaustion.
	OOMContext string `json:"oomContext,omitempty"`
	// LastLogLines are the final log lines captured before container termination.
	LastLogLines []string `json:"lastLogLines,omitempty"`
}

// RSSDissection breaks down the Resident Set Size usage.
type RSSDissection struct {
	AnonRSSBytes    int64 `json:"anonRssBytes,omitempty"`
	FileRSSBytes    int64 `json:"fileRssBytes,omitempty"`
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
