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

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

func addressReconciler(c client.Client) *IPAddressReconciler {
	return &IPAddressReconciler{
		Client:   c,
		Scheme:   c.Scheme(),
		Recorder: record.NewFakeRecorder(100),
	}
}

func reconcileAddress(t *testing.T, r *IPAddressReconciler, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func unboundAddress(name, ip string) *localv1alpha1.IPAddress {
	return &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: testClassName,
			Address:   ip,
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
	}
}

func TestUnboundAddressBecomesAvailable(t *testing.T) {
	c := testClient(t, unboundAddress("ip-1", "203.0.113.1"))
	reconcileAddress(t, addressReconciler(c), "ip-1")

	got := getAddress(t, c, "ip-1")
	if got.Status.Phase != localv1alpha1.IPAddressAvailable {
		t.Errorf("phase = %q, want Available", got.Status.Phase)
	}
	if !hasFinalizer(got.Finalizers, localv1alpha1.AddressProtectionFinalizer) {
		t.Error("address protection finalizer missing")
	}
}

func TestBoundAddressWithLiveClaimBecomesBound(t *testing.T) {
	claim := pendingClaim(testClassName)
	addr := unboundAddress("ip-2", "203.0.113.2")
	addr.Spec.ClaimRef = &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName, UID: testClaimUID}
	c := testClient(t, claim, addr)
	reconcileAddress(t, addressReconciler(c), "ip-2")

	if got := getAddress(t, c, "ip-2"); got.Status.Phase != localv1alpha1.IPAddressBound {
		t.Errorf("phase = %q, want Bound", got.Status.Phase)
	}
}

func TestPreBoundAddressWithoutUIDStaysUntouched(t *testing.T) {
	claim := pendingClaim(testClassName)
	addr := unboundAddress("ip-3", "203.0.113.3")
	addr.Spec.ClaimRef = &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName}
	c := testClient(t, claim, addr)
	reconcileAddress(t, addressReconciler(c), "ip-3")

	if got := getAddress(t, c, "ip-3"); got.Status.Phase == localv1alpha1.IPAddressBound {
		t.Error("phase Bound before the claim controller completed the binding UID")
	}
}

func TestOrphanedAddressRetainIsReleased(t *testing.T) {
	addr := unboundAddress("ip-4", "203.0.113.4")
	addr.Spec.ReclaimPolicy = localv1alpha1.ReclaimRetain
	addr.Spec.ClaimRef = &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: "gone", UID: "old-uid"}
	c := testClient(t, addr)
	reconcileAddress(t, addressReconciler(c), "ip-4")

	got := getAddress(t, c, "ip-4")
	if got.Status.Phase != localv1alpha1.IPAddressReleased {
		t.Errorf("phase = %q, want Released", got.Status.Phase)
	}
	if got.Spec.ClaimRef == nil {
		t.Error("claimRef cleared; it must survive a Retain reclaim")
	}
}

func TestOrphanedAddressDeletePolicyIsDeleted(t *testing.T) {
	addr := unboundAddress("ip-5", "203.0.113.5")
	addr.Spec.ReclaimPolicy = localv1alpha1.ReclaimDelete
	addr.Spec.ClaimRef = &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: "gone", UID: "old-uid"}
	c := testClient(t, addr)
	r := addressReconciler(c)
	reconcileAddress(t, r, "ip-5")
	// The protection finalizer added in the same pass holds the object in
	// deleting state; the deletion reconcile then releases it.
	reconcileAddress(t, r, "ip-5")

	err := c.Get(context.Background(), types.NamespacedName{Name: "ip-5"}, &localv1alpha1.IPAddress{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("address still present: %v", err)
	}
}

func TestUIDMismatchTriggersReclaim(t *testing.T) {
	claim := pendingClaim(testClassName) // UID claim-uid-1
	addr := unboundAddress("ip-6", "203.0.113.6")
	addr.Spec.ClaimRef = &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName, UID: "some-older-uid"}
	c := testClient(t, claim, addr)
	reconcileAddress(t, addressReconciler(c), "ip-6")

	if got := getAddress(t, c, "ip-6"); got.Status.Phase != localv1alpha1.IPAddressReleased {
		t.Errorf("phase = %q, want Released (stale binding to a recreated claim)", got.Status.Phase)
	}
}

func TestStickyPhasesAreNotOverwritten(t *testing.T) {
	for _, phase := range []localv1alpha1.IPAddressPhase{localv1alpha1.IPAddressConflict, localv1alpha1.IPAddressLost} {
		addr := unboundAddress("ip-sticky", "203.0.113.20")
		addr.Finalizers = []string{localv1alpha1.AddressProtectionFinalizer}
		addr.Status.Phase = phase
		c := testClient(t, addr)
		reconcileAddress(t, addressReconciler(c), "ip-sticky")

		if got := getAddress(t, c, "ip-sticky"); got.Status.Phase != phase {
			t.Errorf("phase = %q, want driver-owned %q untouched", got.Status.Phase, phase)
		}
	}
}

func TestDeletionBlockedWhileBoundToLiveClaim(t *testing.T) {
	claim := pendingClaim(testClassName)
	addr := unboundAddress("ip-7", "203.0.113.7")
	addr.Finalizers = []string{localv1alpha1.AddressProtectionFinalizer}
	addr.Spec.ClaimRef = &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName, UID: testClaimUID}
	addr.Status.Phase = localv1alpha1.IPAddressBound
	c := testClient(t, claim, addr)
	if err := c.Delete(context.Background(), addr); err != nil {
		t.Fatal(err)
	}
	reconcileAddress(t, addressReconciler(c), "ip-7")

	got := getAddress(t, c, "ip-7")
	if !hasFinalizer(got.Finalizers, localv1alpha1.AddressProtectionFinalizer) {
		t.Error("finalizer removed while a live claim still holds the address")
	}
}

func TestInvalidAddressGoesPending(t *testing.T) {
	c := testClient(t, unboundAddress("ip-bad", "not-an-ip"))
	reconcileAddress(t, addressReconciler(c), "ip-bad")

	if got := getAddress(t, c, "ip-bad"); got.Status.Phase != localv1alpha1.IPAddressPending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
}
