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

// HarborProjectSpec defines the desired state of HarborProject
type HarborProjectSpec struct {
	// ConnectionRef selects the HarborConnection describing the target Harbor.
	//
	// Immutable: pointing an existing resource at a different Harbor would leave
	// the project behind on the original instance while status.projectID still
	// refers to it.
	//
	// To move a project to a different Harbor, delete and recreate this resource.
	// Set deletionPolicy to Orphan first, otherwise deletion removes the project
	// from the original Harbor.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="connectionRef is immutable"
	ConnectionRef ConnectionReference `json:"connectionRef"`

	// ProjectName is the project name in Harbor. Immutable once created.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="projectName is immutable"
	ProjectName string `json:"projectName"`

	// Public controls project visibility. When unset the controller does not
	// manage this attribute and leaves whatever Harbor has.
	// +optional
	Public *bool `json:"public,omitempty"`

	// AdoptExisting allows the controller to take ownership of a project that
	// already exists in Harbor. Without it, finding a project with the same name
	// is an error.
	//
	// This is only evaluated while the resource is not yet bound to a Harbor
	// project (status.projectID unset). Once bound, clearing this field does not
	// release the project; delete the resource with deletionPolicy Orphan instead.
	// +optional
	// +kubebuilder:default=false
	AdoptExisting bool `json:"adoptExisting,omitempty"`

	// DeletionPolicy decides what happens to the Harbor project when this
	// resource is deleted. Orphan leaves the project in Harbor; Delete removes it.
	//
	// Note that recreating a resource is the only way to change connectionRef or
	// projectName, both of which are immutable. Switch to Orphan before deleting
	// if the Harbor project should survive.
	// +optional
	// +kubebuilder:default=Orphan
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// +kubebuilder:validation:Enum=Delete;Orphan
type DeletionPolicy string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyOrphan DeletionPolicy = "Orphan"
)

// ConnectionReference selects a HarborConnection by name. HarborConnection is
// cluster-scoped, so no namespace is needed.
type ConnectionReference struct {
	Name string `json:"name"`
}

// HarborProjectStatus defines the observed state of HarborProject.
type HarborProjectStatus struct {
	// Conditions represent the current state of the HarborProject.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation the controller last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ProjectID is the numeric ID of the project in Harbor. It is the primary
	// handle for reconciliation; when unset the controller falls back to a
	// lookup by name.
	// +optional
	ProjectID *int64 `json:"projectID,omitempty"`

	// RepositoryCount is the number of repositories in the project, used to
	// explain why deletion is blocked.
	// +optional
	RepositoryCount *int64 `json:"repositoryCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=harbor
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectName`
// +kubebuilder:printcolumn:name="ID",type=integer,JSONPath=`.status.projectID`
// +kubebuilder:printcolumn:name="Public",type=boolean,JSONPath=`.spec.public`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Connection",type=string,JSONPath=`.spec.connectionRef.name`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1
// +kubebuilder:printcolumn:name="Deletion",type=string,JSONPath=`.spec.deletionPolicy`,priority=1

// HarborProject is the Schema for the harborprojects API
type HarborProject struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of HarborProject
	// +required
	Spec HarborProjectSpec `json:"spec"`

	// status defines the observed state of HarborProject
	// +optional
	Status HarborProjectStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HarborProjectList contains a list of HarborProject
type HarborProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HarborProject `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &HarborProject{}, &HarborProjectList{})
		return nil
	})
}
