package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version for this package.
	GroupVersion = schema.GroupVersion{Group: "autopsy.tty.se", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionResource scheme.
	// This uses apimachinery's builder rather than controller-runtime's
	// pkg/scheme so that importing this API package does not drag in
	// controller-runtime as a dependency.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &PodCrashReport{}, &PodCrashReportList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
