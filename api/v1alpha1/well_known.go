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

// Well-known annotations and finalizers of the local.sdn.cozystack.io group.
// Together with the CRD schemas these constants are the contract between the
// core controller and the per-class drivers.
const (
	// IsDefaultClassAnnotation marks an IPAddressClass as the class for
	// claims that do not name one. Value must be "true". The analogue of
	// storageclass.kubernetes.io/is-default-class.
	IsDefaultClassAnnotation = "ipaddressclass.local.sdn.cozystack.io/is-default-class"

	// ProvisionerAnnotation is stamped onto an IPAddressClaim by the core
	// controller once the claim's class is resolved. Its value is the
	// class's spec.provisioner. Per-class drivers watch claims carrying
	// their own name here and provision addresses for the pending ones —
	// the analogue of volume.kubernetes.io/storage-provisioner.
	ProvisionerAnnotation = "local.sdn.cozystack.io/provisioner"

	// ServiceClaimAnnotation, set on a Service by a tenant, names an
	// IPAddressClaim in the Service's own namespace whose address should be
	// pinned to the Service. Consumed by per-class drivers (which translate
	// it into the backend's raw pin annotation), never by tenants writing
	// backend annotations directly.
	ServiceClaimAnnotation = "local.sdn.cozystack.io/ip-address-claim"

	// ClaimProtectionFinalizer is placed on every IPAddressClaim by the
	// core controller so that claim deletion runs the reclaim flow for the
	// bound addresses before the claim disappears.
	ClaimProtectionFinalizer = "local.sdn.cozystack.io/claim-protection"

	// AddressProtectionFinalizer is placed on every IPAddress by the core
	// controller. It blocks deletion of an address that is still bound to a
	// live claim. Drivers add their own finalizer on addresses they
	// provision to tear down the backend allocation.
	AddressProtectionFinalizer = "local.sdn.cozystack.io/address-protection"
)

// Condition types and reasons used by the core controller.
const (
	// ConditionClassResolved reports whether the claim's class reference
	// resolved to an existing IPAddressClass.
	ConditionClassResolved = "ClassResolved"
	// ConditionBound reports whether every requested family is bound.
	ConditionBound = "Bound"

	ReasonResolved               = "Resolved"
	ReasonClassNotFound          = "ClassNotFound"
	ReasonNoDefaultClass         = "NoDefaultClass"
	ReasonMultipleDefaultClasses = "MultipleDefaultClasses"
	ReasonBound                  = "Bound"
	ReasonWaitingForProvisioning = "WaitingForProvisioning"
	ReasonAddressLost            = "AddressLost"
)
