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
)

// AddressFamily selects which IP families a claim requests.
// +kubebuilder:validation:Enum=IPv4;IPv6;Dual
type AddressFamily string

const (
	// FamilyIPv4 requests one IPv4 address.
	FamilyIPv4 AddressFamily = "IPv4"
	// FamilyIPv6 requests one IPv6 address.
	FamilyIPv6 AddressFamily = "IPv6"
	// FamilyDual requests one IPv4 and one IPv6 address. A Dual claim binds
	// two IPAddress objects and reports both in status.addresses.
	FamilyDual AddressFamily = "Dual"
)

// IPAddressClaimSpec defines the desired state of IPAddressClaim.
type IPAddressClaimSpec struct {
	// className names the IPAddressClass to draw from. Empty means the
	// default class (the one annotated
	// ipaddressclass.local.sdn.cozystack.io/is-default-class: "true").
	// +optional
	ClassName string `json:"className,omitempty"`

	// family selects the IP families requested.
	// +optional
	// +kubebuilder:default=IPv4
	Family AddressFamily `json:"family,omitempty"`

	// addressName pins the claim to a specific Available IPAddress instead
	// of letting the controller match or the driver provision one — the
	// analogue of PersistentVolumeClaim.spec.volumeName. Only meaningful
	// for single-family claims.
	// +optional
	AddressName string `json:"addressName,omitempty"`
}

// IPAddressClaimPhase describes the lifecycle phase of a claim.
// +kubebuilder:validation:Enum=Pending;Bound;Lost
type IPAddressClaimPhase string

const (
	// ClaimPending means the claim is not yet fully bound.
	ClaimPending IPAddressClaimPhase = "Pending"
	// ClaimBound means every requested family is bound to an IPAddress.
	ClaimBound IPAddressClaimPhase = "Bound"
	// ClaimLost means an address the claim was bound to disappeared or was
	// rebound elsewhere.
	ClaimLost IPAddressClaimPhase = "Lost"
)

// BoundAddress reports one IPAddress bound to the claim.
type BoundAddress struct {
	// name of the bound IPAddress object.
	Name string `json:"name"`
	// address is the IP itself — what the tenant reads and puts in DNS.
	Address string `json:"address"`
}

// IPAddressClaimStatus defines the observed state of IPAddressClaim.
type IPAddressClaimStatus struct {
	// phase is the current lifecycle phase of the claim.
	// +optional
	Phase IPAddressClaimPhase `json:"phase,omitempty"`

	// className is the class the claim resolved to. For claims created
	// without a className it records which default class was picked; the
	// resolution is sticky.
	// +optional
	ClassName string `json:"className,omitempty"`

	// addresses lists the bound addresses. A list deliberately: a Dual
	// claim binds a v4 and a v6 IPAddress and must report both. For a
	// single-family claim the list has one entry.
	// +optional
	Addresses []BoundAddress `json:"addresses,omitempty"`

	// conditions represent the latest available observations of the
	// claim's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.status.className`
// +kubebuilder:printcolumn:name="Addresses",type=string,JSONPath=`.status.addresses[*].address`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IPAddressClaim is a namespaced request for an address — the whole
// tenant-facing API, the PersistentVolumeClaim of the address model.
type IPAddressClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPAddressClaimSpec   `json:"spec,omitempty"`
	Status IPAddressClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IPAddressClaimList contains a list of IPAddressClaim.
type IPAddressClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPAddressClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&IPAddressClaim{}, &IPAddressClaimList{})
}
