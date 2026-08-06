/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ConfigSyncSpec defines the desired state of ConfigSync
type ConfigSyncSpec struct {
	// targetNamespaces is the list of namespaces the managed ConfigMap is
	// materialized into. Removing a namespace from this list causes the
	// ConfigMap in that namespace to be pruned on the next reconcile.
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	// +required
	TargetNamespaces []string `json:"targetNamespaces"`

	// data is the key/value payload written into the .data of the managed
	// ConfigMap in every target namespace.
	// +kubebuilder:validation:MinProperties=1
	// +required
	Data map[string]string `json:"data"`
}

// ConfigSyncStatus defines the observed state of ConfigSync.
type ConfigSyncStatus struct {
	// observedGeneration is the .metadata.generation of the ConfigSync that the
	// controller last reconciled. When it equals .metadata.generation, the
	// reported status reflects the current spec.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// syncedNamespaces lists the namespaces where the managed ConfigMap is
	// currently present and converged. Namespaces skipped because a foreign
	// ConfigMap already owns the name are not listed here.
	// +listType=atomic
	// +optional
	SyncedNamespaces []string `json:"syncedNamespaces,omitempty"`

	// conditions represent the current state of the ConfigSync resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Condition types used by this controller:
	// - "Ready": True when the managed ConfigMap is converged in every target
	//   namespace. False when a namespace is missing, a name is already taken by
	//   a ConfigMap this controller does not own, or a write failed.
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=".status.syncedNamespaces"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be 63 characters or fewer, because it is copied into a label value on every managed ConfigMap"

// ConfigSync is the Schema for the configsyncs API
type ConfigSync struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ConfigSync
	// +required
	Spec ConfigSyncSpec `json:"spec"`

	// status defines the observed state of ConfigSync
	// +optional
	Status ConfigSyncStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ConfigSyncList contains a list of ConfigSync
type ConfigSyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ConfigSync `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ConfigSync{}, &ConfigSyncList{})
		return nil
	})
}
