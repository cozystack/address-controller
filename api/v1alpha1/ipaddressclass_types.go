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
	"k8s.io/apimachinery/pkg/api/meta"
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

	// releaseDelay, under reclaimPolicy Retain, returns a Released address
	// of this class to Available once it has been Released this long: the
	// core controller clears its claimRef. Unset keeps a Released address
	// until an admin clears it. An address annotated
	// ipaddress.local.sdn.cozystack.io/held: "true" is exempt.
	// +optional
	ReleaseDelay *metav1.Duration `json:"releaseDelay,omitempty"`

	// parameters is an opaque, driver-specific configuration blob (for
	// example the address ranges to carve from). The core controller never
	// interprets it.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Parameters *runtime.RawExtension `json:"parameters,omitempty"`
}

// IPAddressClassStatus is written by the provisioner that serves the class.
// The core controller never writes it.
type IPAddressClassStatus struct {
	// loadBalancerClass is the spec.loadBalancerClass a Service consuming a
	// claim of this class must carry, so that the Service reaches the
	// load-balancer implementation the provisioner fronts. Empty means the
	// cluster's default implementation, and such a Service carries none. It
	// is meaningful only while the Accepted condition is True for the
	// class's current generation: before that, an empty value is not yet
	// reported rather than the default.
	// +optional
	LoadBalancerClass string `json:"loadBalancerClass,omitempty"`

	// conditions holds the provisioner's Accepted condition.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provisioner",type=string,JSONPath=`.spec.provisioner`
// +kubebuilder:printcolumn:name="ReclaimPolicy",type=string,JSONPath=`.spec.reclaimPolicy`
// +kubebuilder:printcolumn:name="LoadBalancerClass",type=string,JSONPath=`.status.loadBalancerClass`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IPAddressClass describes an address source: which pool, which driver.
// It is the StorageClass of the address model. Marking a class with the
// annotation ipaddressclass.local.sdn.cozystack.io/is-default-class: "true"
// makes it the class for claims that do not name one.
type IPAddressClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPAddressClassSpec   `json:"spec,omitempty"`
	Status IPAddressClassStatus `json:"status,omitempty"`
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

// AcceptedLoadBalancerClass returns the loadBalancerClass the class's
// provisioner reports, and whether it has reported one: the Accepted
// condition is True for the class's current generation. A consumer that
// creates a Service for a claim of the class waits until ok is true, since
// a Service's loadBalancerClass cannot change after creation.
func (c *IPAddressClass) AcceptedLoadBalancerClass() (lbClass string, ok bool) {
	cond := meta.FindStatusCondition(c.Status.Conditions, ConditionAccepted)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != c.Generation {
		return "", false
	}
	return c.Status.LoadBalancerClass, true
}
