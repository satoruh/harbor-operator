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

// HarborConnectionSpec describes how to reach a Harbor instance.
type HarborConnectionSpec struct {
	// BaseURL is the root URL of the Harbor API, without a trailing slash
	// and without the /api/v2.0 path (for example https://harbor.example.com).
	// +kubebuilder:validation:Pattern=`^https?://`
	// +kubebuilder:validation:MaxLength=2048
	BaseURL string `json:"baseURL"`

	// CredentialsRef selects the Secret holding the Harbor username and
	// password. The Secret must live in the operator's own namespace and
	// carry the label harbor.satoruh.org/credentials=true.
	CredentialsRef SecretKeyReference `json:"credentialsRef"`

	// CABundleRef selects a ConfigMap containing a PEM-encoded CA bundle used
	// to verify the Harbor certificate. Required when Harbor is served with a
	// certificate that is not signed by a system-trusted CA.
	// +optional
	CABundleRef *ConfigMapKeyReference `json:"caBundleRef,omitempty"`

	// InsecureSkipVerify disables TLS certificate verification.
	// Intended for development only; prefer CABundleRef.
	// +optional
	// +kubebuilder:default=false
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// SyncPeriod is how often HarborProject resources bound to this connection
	// are reconciled against Harbor to detect drift. Harbor cannot be watched,
	// so this polling interval bounds how long a manual change persists.
	// +optional
	// +kubebuilder:default="10m"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('30s')",message="syncPeriod must be at least 30s"
	SyncPeriod *metav1.Duration `json:"syncPeriod,omitempty"`
}

// SecretKeyReference points at a Secret in the operator's namespace.
// The namespace is deliberately not configurable: it bounds which Secrets the
// controller can be directed to read.
type SecretKeyReference struct {
	// Name of the Secret.
	Name string `json:"name"`

	// UsernameKey is the key holding the Harbor username. For a robot account
	// this is the full name including the robot$ prefix.
	// +optional
	// +kubebuilder:default=username
	UsernameKey string `json:"usernameKey,omitempty"`

	// PasswordKey is the key holding the Harbor password or robot token.
	// +optional
	// +kubebuilder:default=password
	PasswordKey string `json:"passwordKey,omitempty"`
}

// ConfigMapKeyReference points at a ConfigMap in the operator's namespace.
type ConfigMapKeyReference struct {
	// Name of the ConfigMap.
	Name string `json:"name"`

	// Key holding the PEM-encoded certificate bundle.
	// +optional
	// +kubebuilder:default="ca.crt"
	Key string `json:"key,omitempty"`
}

// HarborConnectionStatus reports reachability of the Harbor instance.
type HarborConnectionStatus struct {
	// Conditions represent the current state of the HarborConnection.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the .metadata.generation the controller last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// HarborVersion is reported by the Harbor instance at the last successful probe.
	// +optional
	HarborVersion string `json:"harborVersion,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=harbor
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.baseURL`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.harborVersion`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,priority=1

// HarborConnection is the Schema for the harborconnections API
type HarborConnection struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of HarborConnection
	// +required
	Spec HarborConnectionSpec `json:"spec"`

	// status defines the observed state of HarborConnection
	// +optional
	Status HarborConnectionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HarborConnectionList contains a list of HarborConnection
type HarborConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HarborConnection `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &HarborConnection{}, &HarborConnectionList{})
		return nil
	})
}
