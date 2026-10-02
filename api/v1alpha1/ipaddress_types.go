/*
Copyright 2026 Timofei Larkin.

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
	"k8s.io/apimachinery/pkg/types"
)

// ClaimReference points an IPAddress at the IPAddressClaim it is bound to.
type ClaimReference struct {
	// namespace of the claim.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// name of the claim.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// uid of the claim. A driver pre-binding an address may leave it empty;
	// the core controller completes it when it accepts the binding.
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// FromClassSource records that the address was carved from the class's own
// range by its driver — the driver is the IPAM of record.
type FromClassSource struct{}

// ProviderReference records that the address wraps a reservation held by an
// external provider (a cloud EIP, a named static address). The driver adopted
// it rather than allocating it.
type ProviderReference struct {
	// id is the provider-side stable handle of the reservation, for example
	// an AWS allocation id ("eipalloc-0a1b...").
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`
}

// IPAddressSource is a union describing where the address came from —
// the analogue of the PersistentVolume volume-source union. Exactly one
// member must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.fromClass) ? 1 : 0) + (has(self.providerRef) ? 1 : 0) == 1",message="exactly one of fromClass or providerRef must be set"
type IPAddressSource struct {
	// fromClass marks the address as allocated from the class's range.
	// +optional
	FromClass *FromClassSource `json:"fromClass,omitempty"`
	// providerRef marks the address as adopted from a provider-side
	// reservation.
	// +optional
	ProviderRef *ProviderReference `json:"providerRef,omitempty"`
}

// IPAddressSpec defines the desired state of IPAddress.
type IPAddressSpec struct {
	// className names the IPAddressClass this address belongs to.
	// +kubebuilder:validation:MinLength=1
	ClassName string `json:"className"`

	// address is the IP address itself, in canonical textual form
	// (e.g. "203.0.113.7" or "2001:db8::7").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="address is immutable"
	Address string `json:"address"`

	// reclaimPolicy governs what happens to this object when its claim is
	// deleted; copied from the class at provisioning time.
	// +optional
	// +kubebuilder:default=Retain
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// claimRef binds this address to a claim. Set either by the driver at
	// provisioning time (pre-bound) or by the core controller when it
	// matches an Available address to a claim. An address with no claimRef
	// is Available. After a Retain reclaim the field survives the claim
	// (phase Released) until an admin clears it.
	// +optional
	ClaimRef *ClaimReference `json:"claimRef,omitempty"`

	// source records where the address came from.
	Source IPAddressSource `json:"source"`
}

// IPAddressPhase describes the lifecycle phase of an IPAddress.
// +kubebuilder:validation:Enum=Pending;Available;Bound;Released;Conflict;Lost
type IPAddressPhase string

const (
	// IPAddressPending means the object has not been reconciled yet or its
	// spec does not validate.
	IPAddressPending IPAddressPhase = "Pending"
	// IPAddressAvailable means the address is not bound to any claim and
	// may be matched to one.
	IPAddressAvailable IPAddressPhase = "Available"
	// IPAddressBound means the address is bound to a live claim. A Bound
	// address with no status.associatedTo is reserved but inert — held,
	// attached to nothing.
	IPAddressBound IPAddressPhase = "Bound"
	// IPAddressReleased means the claim was deleted under reclaimPolicy
	// Retain. The address keeps its claimRef and is not reusable until an
	// admin clears it.
	IPAddressReleased IPAddressPhase = "Released"
	// IPAddressConflict means a live Service holds this address although
	// the address's binding does not authorize it. Set by drivers; the core
	// controller treats it as sticky.
	IPAddressConflict IPAddressPhase = "Conflict"
	// IPAddressLost means the backing allocation disappeared (for example a
	// provider-side reservation was released). Set by drivers; the core
	// controller treats it as sticky.
	IPAddressLost IPAddressPhase = "Lost"
)

// AssociationReference names the workload object an address is currently
// associated with (announced for). Maintained by the per-class driver.
type AssociationReference struct {
	// kind of the associated object, e.g. "Service".
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`
	// namespace of the associated object.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// name of the associated object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// IPAddressStatus defines the observed state of IPAddress.
type IPAddressStatus struct {
	// phase is the current lifecycle phase of the address.
	// +optional
	Phase IPAddressPhase `json:"phase,omitempty"`

	// associatedTo names the workload the address is currently announced
	// for. Nil means reserved but inert. Maintained by the per-class
	// driver, never by the core controller.
	// +optional
	AssociatedTo *AssociationReference `json:"associatedTo,omitempty"`

	// conditions represent the latest available observations of the
	// address's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.className`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="ClaimNamespace",type=string,JSONPath=`.spec.claimRef.namespace`
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.spec.claimRef.name`
// +kubebuilder:printcolumn:name="AttachedTo",type=string,JSONPath=`.status.associatedTo.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IPAddress is a concrete address the cluster owns — the inventory object,
// the PersistentVolume of the address model. Created by a per-class driver
// (or an admin, for static pre-provisioning), never by tenants.
type IPAddress struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPAddressSpec   `json:"spec,omitempty"`
	Status IPAddressStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IPAddressList contains a list of IPAddress.
type IPAddressList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPAddress `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IPAddress{}, &IPAddressList{})
}
