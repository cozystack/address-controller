/*
Copyright 2026 The Cozystack Authors.

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

// ReclaimPolicy describes what happens to an IPAddress when the claim
// bound to it is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type ReclaimPolicy string

const (
	// ReclaimRetain keeps the IPAddress after its claim is deleted. The
	// address moves to phase Released and is not reusable until an admin
	// clears its claimRef.
	ReclaimRetain ReclaimPolicy = "Retain"
	// ReclaimDelete deletes the IPAddress after its claim is deleted. The
	// per-class driver tears down the backend allocation via its finalizer
	// before the object is removed.
	ReclaimDelete ReclaimPolicy = "Delete"
)

// IPAddressClassSpec defines the desired state of IPAddressClass.
type IPAddressClassSpec struct {
	// provisioner names the per-class driver that fulfils claims of this
	// class, exactly as StorageClass.provisioner names a CSI driver. The
	// core controller never provisions addresses itself; it stamps this
	// value onto pending claims so the named driver can pick them up.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provisioner is immutable"
	Provisioner string `json:"provisioner"`

	// reclaimPolicy is copied to IPAddress objects provisioned for this
	// class and governs what happens to them when their claim is deleted.
	// +optional
	// +kubebuilder:default=Retain
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// parameters is an opaque, driver-specific configuration blob (for
	// example the address ranges to carve from). The core controller never
	// interprets it.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Parameters *runtime.RawExtension `json:"parameters,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Provisioner",type=string,JSONPath=`.spec.provisioner`
// +kubebuilder:printcolumn:name="ReclaimPolicy",type=string,JSONPath=`.spec.reclaimPolicy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IPAddressClass describes an address source: which pool, which driver.
// It is the StorageClass of the address model. Marking a class with the
// annotation ipaddressclass.local.sdn.cozystack.io/is-default-class: "true"
// makes it the class for claims that do not name one.
type IPAddressClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec IPAddressClassSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// IPAddressClassList contains a list of IPAddressClass.
type IPAddressClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPAddressClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IPAddressClass{}, &IPAddressClassList{})
}
