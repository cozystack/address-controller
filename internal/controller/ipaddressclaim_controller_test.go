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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

const (
	testClassName   = "public"
	otherClassName  = "private"
	testProvisioner = "metallb.drivers.local.sdn.cozystack.io"
	testNamespace   = "tenant-a"
	testClaimName   = "web"
	testClaimUID    = "claim-uid-1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := localv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func testClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&localv1alpha1.IPAddress{}, &localv1alpha1.IPAddressClaim{}).
		WithIndex(&localv1alpha1.IPAddress{}, IPAddressClaimRefIndex, claimRefIndex).
		WithIndex(&localv1alpha1.IPAddress{}, IPAddressClassNameIndex, classNameIndex).
		WithObjects(objs...).
		Build()
}

func claimReconciler(c client.Client) *IPAddressClaimReconciler {
	return &IPAddressClaimReconciler{
		Client:   c,
		Scheme:   c.Scheme(),
		Recorder: record.NewFakeRecorder(100),
	}
}

func reconcileClaim(t *testing.T, r *IPAddressClaimReconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testClaimName},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getClaim(t *testing.T, c client.Client) *localv1alpha1.IPAddressClaim {
	t.Helper()
	claim := &localv1alpha1.IPAddressClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testClaimName}, claim); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	return claim
}

func getAddress(t *testing.T, c client.Client, name string) *localv1alpha1.IPAddress {
	t.Helper()
	addr := &localv1alpha1.IPAddress{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, addr); err != nil {
		t.Fatalf("get address %s: %v", name, err)
	}
	return addr
}

func publicClass(annotations map[string]string) *localv1alpha1.IPAddressClass {
	return &localv1alpha1.IPAddressClass{
		ObjectMeta: metav1.ObjectMeta{Name: testClassName, Annotations: annotations},
		Spec: localv1alpha1.IPAddressClassSpec{
			Provisioner:   testProvisioner,
			ReclaimPolicy: localv1alpha1.ReclaimRetain,
		},
	}
}

func defaultClass() *localv1alpha1.IPAddressClass {
	return publicClass(map[string]string{localv1alpha1.IsDefaultClassAnnotation: localv1alpha1.AnnotationTrue})
}

func pendingClaim(className string) *localv1alpha1.IPAddressClaim {
	return &localv1alpha1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testClaimName, Namespace: testNamespace, UID: testClaimUID},
		Spec: localv1alpha1.IPAddressClaimSpec{
			ClassName: className,
			Family:    localv1alpha1.FamilyIPv4,
		},
	}
}

func TestClaimResolvesClassAndWaitsForProvisioner(t *testing.T) {
	c := testClient(t, publicClass(nil), pendingClaim(testClassName))
	reconcileClaim(t, claimReconciler(c))

	claim := getClaim(t, c)
	if claim.Status.Phase != localv1alpha1.ClaimPending {
		t.Errorf("phase = %q, want Pending", claim.Status.Phase)
	}
	if got := claim.Annotations[localv1alpha1.ProvisionerAnnotation]; got != testProvisioner {
		t.Errorf("provisioner annotation = %q", got)
	}
	if claim.Status.ClassName != testClassName {
		t.Errorf("status.className = %q", claim.Status.ClassName)
	}
	if !hasFinalizer(claim.Finalizers, localv1alpha1.ClaimProtectionFinalizer) {
		t.Error("claim protection finalizer missing")
	}
	cond := meta.FindStatusCondition(claim.Status.Conditions, localv1alpha1.ConditionBound)
	if cond == nil || cond.Reason != localv1alpha1.ReasonWaitingForProvisioning {
		t.Errorf("Bound condition = %+v, want reason WaitingForProvisioning", cond)
	}
}

func TestClaimUsesDefaultClass(t *testing.T) {
	c := testClient(t,
		defaultClass(),
		pendingClaim(""))
	reconcileClaim(t, claimReconciler(c))

	claim := getClaim(t, c)
	if claim.Status.ClassName != testClassName {
		t.Errorf("status.className = %q, want public (the default class)", claim.Status.ClassName)
	}
}

func TestClaimWithoutAnyDefaultClassStaysPending(t *testing.T) {
	c := testClient(t, publicClass(nil), pendingClaim(""))
	reconcileClaim(t, claimReconciler(c))

	claim := getClaim(t, c)
	cond := meta.FindStatusCondition(claim.Status.Conditions, localv1alpha1.ConditionClassResolved)
	if cond == nil || cond.Reason != localv1alpha1.ReasonNoDefaultClass {
		t.Errorf("ClassResolved condition = %+v, want reason NoDefaultClass", cond)
	}
}

func TestClaimWithMultipleDefaultClassesStaysPending(t *testing.T) {
	second := defaultClass()
	second.Name = "public-2"
	c := testClient(t,
		defaultClass(),
		second,
		pendingClaim(""))
	reconcileClaim(t, claimReconciler(c))

	claim := getClaim(t, c)
	cond := meta.FindStatusCondition(claim.Status.Conditions, localv1alpha1.ConditionClassResolved)
	if cond == nil || cond.Reason != localv1alpha1.ReasonMultipleDefaultClasses {
		t.Errorf("ClassResolved condition = %+v, want reason MultipleDefaultClasses", cond)
	}
}

func TestClaimAcceptsDriverPreBoundAddress(t *testing.T) {
	addr := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-203-0-113-7"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: testClassName,
			Address:   "203.0.113.7",
			// Driver pre-binds without a UID; the core controller completes it.
			ClaimRef: &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName},
			Source:   localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
	}
	c := testClient(t, publicClass(nil), pendingClaim(testClassName), addr)
	reconcileClaim(t, claimReconciler(c))

	claim := getClaim(t, c)
	if claim.Status.Phase != localv1alpha1.ClaimBound {
		t.Fatalf("phase = %q, want Bound", claim.Status.Phase)
	}
	if len(claim.Status.Addresses) != 1 || claim.Status.Addresses[0].Address != "203.0.113.7" {
		t.Errorf("status.addresses = %+v", claim.Status.Addresses)
	}
	if got := getAddress(t, c, "ip-203-0-113-7").Spec.ClaimRef.UID; got != testClaimUID {
		t.Errorf("claimRef.uid = %q, want completed to claim-uid-1", got)
	}
}

func TestClaimMatchesAvailableAddress(t *testing.T) {
	addr := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-203-0-113-8"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: testClassName,
			Address:   "203.0.113.8",
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressAvailable},
	}
	c := testClient(t, publicClass(nil), pendingClaim(testClassName), addr)
	reconcileClaim(t, claimReconciler(c))

	claim := getClaim(t, c)
	if claim.Status.Phase != localv1alpha1.ClaimBound {
		t.Fatalf("phase = %q, want Bound", claim.Status.Phase)
	}
	ref := getAddress(t, c, "ip-203-0-113-8").Spec.ClaimRef
	if ref == nil || ref.Namespace != testNamespace || ref.Name != testClaimName || ref.UID != testClaimUID {
		t.Errorf("claimRef = %+v", ref)
	}
}

func TestClaimIgnoresAvailableAddressOfWrongClassOrFamily(t *testing.T) {
	wrongClass := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-wrong-class"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: "other",
			Address:   "203.0.113.9",
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressAvailable},
	}
	wrongFamily := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-wrong-family"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: testClassName,
			Address:   "2001:db8::9",
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressAvailable},
	}
	c := testClient(t, publicClass(nil), pendingClaim(testClassName), wrongClass, wrongFamily)
	reconcileClaim(t, claimReconciler(c))

	if claim := getClaim(t, c); claim.Status.Phase != localv1alpha1.ClaimPending {
		t.Errorf("phase = %q, want Pending", claim.Status.Phase)
	}
}

func TestDualClaimNeedsBothFamilies(t *testing.T) {
	claim := pendingClaim(testClassName)
	claim.Spec.Family = localv1alpha1.FamilyDual
	v4 := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-v4"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: testClassName,
			Address:   "203.0.113.10",
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressAvailable},
	}
	c := testClient(t, publicClass(nil), claim, v4)
	r := claimReconciler(c)
	reconcileClaim(t, r)

	got := getClaim(t, c)
	if got.Status.Phase != localv1alpha1.ClaimPending {
		t.Fatalf("phase = %q, want Pending (v6 still missing)", got.Status.Phase)
	}
	if len(got.Status.Addresses) != 1 {
		t.Fatalf("status.addresses = %+v, want the v4 half reported", got.Status.Addresses)
	}

	v6 := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-v6"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: testClassName,
			Address:   "2001:db8::10",
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressAvailable},
	}
	if err := c.Create(context.Background(), v6); err != nil {
		t.Fatal(err)
	}
	reconcileClaim(t, r)

	got = getClaim(t, c)
	if got.Status.Phase != localv1alpha1.ClaimBound {
		t.Fatalf("phase = %q, want Bound", got.Status.Phase)
	}
	if len(got.Status.Addresses) != 2 {
		t.Errorf("status.addresses = %+v, want both families", got.Status.Addresses)
	}
}

func TestClaimDeletionRetainReleasesAddress(t *testing.T) {
	claim := pendingClaim(testClassName)
	claim.Finalizers = []string{localv1alpha1.ClaimProtectionFinalizer}
	addr := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-retained"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName:     testClassName,
			Address:       "203.0.113.11",
			ReclaimPolicy: localv1alpha1.ReclaimRetain,
			ClaimRef:      &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName, UID: testClaimUID},
			Source:        localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressBound},
	}
	c := testClient(t, publicClass(nil), claim, addr)
	if err := c.Delete(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	reconcileClaim(t, claimReconciler(c))

	err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testClaimName}, &localv1alpha1.IPAddressClaim{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("claim still present after finalization: %v", err)
	}
	got := getAddress(t, c, "ip-retained")
	if got.Status.Phase != localv1alpha1.IPAddressReleased {
		t.Errorf("address phase = %q, want Released", got.Status.Phase)
	}
	if got.Spec.ClaimRef == nil {
		t.Error("claimRef cleared on Retain; it must survive until an admin clears it")
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, localv1alpha1.ConditionBound); cond == nil || cond.Reason != localv1alpha1.ReasonReleased {
		t.Errorf("Bound condition = %+v, want reason Released to date the release", cond)
	}
}

func TestClaimDeletionDeletePolicyDeletesAddress(t *testing.T) {
	claim := pendingClaim(testClassName)
	claim.Finalizers = []string{localv1alpha1.ClaimProtectionFinalizer}
	addr := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-deleted"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName:     testClassName,
			Address:       "203.0.113.12",
			ReclaimPolicy: localv1alpha1.ReclaimDelete,
			ClaimRef:      &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName, UID: testClaimUID},
			Source:        localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressBound},
	}
	c := testClient(t, publicClass(nil), claim, addr)
	if err := c.Delete(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	reconcileClaim(t, claimReconciler(c))

	err := c.Get(context.Background(), types.NamespacedName{Name: "ip-deleted"}, &localv1alpha1.IPAddress{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("address still present after Delete reclaim: %v", err)
	}
}

func TestControllerOwnedClaimDeletionDeletesRetainedAddress(t *testing.T) {
	// The owner rule holds for any controller owner, not only a Service.
	for _, owner := range []metav1.OwnerReference{
		{APIVersion: "v1", Kind: serviceKind, Name: "web-lb", UID: testServiceUID, Controller: new(true)},
		{APIVersion: "local.sdn.cozystack.io/v1alpha1", Kind: "EndpointAttachment", Name: "web", UID: "ea-uid-1", Controller: new(true)},
	} {
		t.Run(owner.Kind, func(t *testing.T) {
			claim := pendingClaim(testClassName)
			claim.Finalizers = []string{localv1alpha1.ClaimProtectionFinalizer}
			claim.OwnerReferences = []metav1.OwnerReference{owner}
			addr := boundAddress("ip-owned", "203.0.113.13", localv1alpha1.IPAddressBound)
			c := testClient(t, publicClass(nil), claim, addr)
			if err := c.Delete(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			reconcileClaim(t, claimReconciler(c))

			err := c.Get(context.Background(), types.NamespacedName{Name: "ip-owned"}, &localv1alpha1.IPAddress{})
			if !apierrors.IsNotFound(err) {
				t.Errorf("address of a %s-owned claim survived the claim under Retain: %v", owner.Kind, err)
			}
		})
	}
}

func TestClaimDeletionLeavesConflictAndLostUntouched(t *testing.T) {
	for _, phase := range []localv1alpha1.IPAddressPhase{localv1alpha1.IPAddressConflict, localv1alpha1.IPAddressLost} {
		t.Run(string(phase), func(t *testing.T) {
			claim := pendingClaim(testClassName)
			claim.Finalizers = []string{localv1alpha1.ClaimProtectionFinalizer}
			c := testClient(t, publicClass(nil), claim, boundAddress("ip-sticky", "203.0.113.15", phase))
			if err := c.Delete(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			reconcileClaim(t, claimReconciler(c))

			if got := getAddress(t, c, "ip-sticky").Status.Phase; got != phase {
				t.Errorf("address phase = %q, want %q kept", got, phase)
			}
		})
	}
}

func TestLostAddressIsNotReplaced(t *testing.T) {
	// A Lost address keeps its family covered: the claim reports Lost and
	// waits instead of binding a second address of the same family.
	claim := pendingClaim(testClassName)
	claim.Finalizers = []string{localv1alpha1.ClaimProtectionFinalizer}
	claim.Status = localv1alpha1.IPAddressClaimStatus{Phase: localv1alpha1.ClaimBound, ClassName: testClassName}
	spare := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-spare"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: testClassName,
			Address:   "203.0.113.17",
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressAvailable},
	}
	c := testClient(t, publicClass(nil), claim, boundAddress("ip-lost", "203.0.113.16", localv1alpha1.IPAddressLost), spare)
	reconcileClaim(t, claimReconciler(c))

	if ref := getAddress(t, c, "ip-spare").Spec.ClaimRef; ref != nil {
		t.Errorf("spare address bound to the claim (%+v); a Lost address must not be replaced", ref)
	}
	got := getClaim(t, c)
	if got.Status.Phase != localv1alpha1.ClaimLost {
		t.Errorf("phase = %q, want Lost", got.Status.Phase)
	}
	if len(got.Status.Addresses) != 1 || got.Status.Addresses[0].Name != "ip-lost" {
		t.Errorf("status.addresses = %+v, want the Lost address kept", got.Status.Addresses)
	}
}

func TestClaimRefusesMismatchedPreBinding(t *testing.T) {
	cases := map[string]*localv1alpha1.IPAddress{
		"other class":        preBoundAddress("203.0.113.18", otherClassName),
		"unrequested family": preBoundAddress("2001:db8::18", testClassName),
	}
	for name, addr := range cases {
		t.Run(name, func(t *testing.T) {
			c := testClient(t, publicClass(nil), pendingClaim(testClassName), addr)
			reconcileClaim(t, claimReconciler(c))

			if got := getAddress(t, c, "ip-pre").Spec.ClaimRef.UID; got != "" {
				t.Errorf("claimRef.uid = %q, want the pre-binding left incomplete", got)
			}
			if got := getClaim(t, c); len(got.Status.Addresses) != 0 {
				t.Errorf("status.addresses = %+v, want the refused address left out", got.Status.Addresses)
			}
		})
	}
}

func TestDefaultClassClaimAcceptsPreBinding(t *testing.T) {
	// A claim naming no class resolves the default before judging a
	// pre-binding against it.
	c := testClient(t, defaultClass(), pendingClaim(""), preBoundAddress("203.0.113.22", testClassName))
	reconcileClaim(t, claimReconciler(c))

	if got := getAddress(t, c, "ip-pre").Spec.ClaimRef.UID; got != testClaimUID {
		t.Errorf("claimRef.uid = %q, want the pre-binding accepted", got)
	}
	if got := getClaim(t, c); got.Status.Phase != localv1alpha1.ClaimBound || got.Status.ClassName != testClassName {
		t.Errorf("phase = %q, className = %q, want Bound to %q", got.Status.Phase, got.Status.ClassName, testClassName)
	}
}

func TestClaimRefusesSecondPreBindingOfAFamily(t *testing.T) {
	held := boundAddress("ip-held", "203.0.113.19", localv1alpha1.IPAddressBound)
	c := testClient(t, publicClass(nil), pendingClaim(testClassName), held,
		preBoundAddress("203.0.113.20", testClassName))
	reconcileClaim(t, claimReconciler(c))

	if got := getAddress(t, c, "ip-pre").Spec.ClaimRef.UID; got != "" {
		t.Errorf("claimRef.uid = %q, want a second IPv4 pre-binding refused", got)
	}
}

func TestOwnedClaimDeletionSparesRefusedPreBinding(t *testing.T) {
	claim := pendingClaim(testClassName)
	claim.Finalizers = []string{localv1alpha1.ClaimProtectionFinalizer}
	claim.Status.ClassName = testClassName
	claim.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: serviceKind, Name: "web-lb", UID: testServiceUID, Controller: new(true),
	}}
	c := testClient(t, publicClass(nil), claim, preBoundAddress("203.0.113.21", otherClassName))
	if err := c.Delete(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	reconcileClaim(t, claimReconciler(c))

	getAddress(t, c, "ip-pre") // fails the test if the address was deleted
}

// boundAddress is an address bound to the test claim, UID included.
func boundAddress(name, ip string, phase localv1alpha1.IPAddressPhase) *localv1alpha1.IPAddress {
	return &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName:     testClassName,
			Address:       ip,
			ReclaimPolicy: localv1alpha1.ReclaimRetain,
			ClaimRef:      &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName, UID: testClaimUID},
			Source:        localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: phase},
	}
}

// preBoundAddress is the address "ip-pre", pre-bound to the test claim by
// name only.
func preBoundAddress(ip, className string) *localv1alpha1.IPAddress {
	return &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-pre"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName:     className,
			Address:       ip,
			ReclaimPolicy: localv1alpha1.ReclaimRetain,
			ClaimRef:      &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName},
			Source:        localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
	}
}

func TestBoundClaimSurvivesClassDeletion(t *testing.T) {
	// A fully bound claim must reconcile without touching its class:
	// deleting the class breaks neither the binding nor status upkeep.
	claim := pendingClaim(testClassName)
	claim.Finalizers = []string{localv1alpha1.ClaimProtectionFinalizer}
	claim.Annotations = map[string]string{localv1alpha1.ProvisionerAnnotation: testProvisioner}
	claim.Status = localv1alpha1.IPAddressClaimStatus{
		Phase:     localv1alpha1.ClaimBound,
		ClassName: testClassName,
	}
	addr := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-203-0-113-14"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: testClassName,
			Address:   "203.0.113.14",
			ClaimRef:  &localv1alpha1.ClaimReference{Namespace: testNamespace, Name: testClaimName, UID: testClaimUID},
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressBound},
	}
	// Note: no IPAddressClass object at all.
	c := testClient(t, claim, addr)
	reconcileClaim(t, claimReconciler(c))

	got := getClaim(t, c)
	if got.Status.Phase != localv1alpha1.ClaimBound {
		t.Errorf("phase = %q, want Bound to survive class deletion", got.Status.Phase)
	}
	if got.Status.ClassName != testClassName {
		t.Errorf("status.className = %q, want the sticky record untouched", got.Status.ClassName)
	}
	if len(got.Status.Addresses) != 1 || got.Status.Addresses[0].Address != "203.0.113.14" {
		t.Errorf("status.addresses = %+v, want status upkeep to continue", got.Status.Addresses)
	}
}

func TestPendingClaimWithMissingClassRecordsClassNotFound(t *testing.T) {
	// An unbound claim naming a class that does not exist stays Pending
	// with the reason recorded; only provisioning is blocked.
	c := testClient(t, pendingClaim("nonexistent"))
	reconcileClaim(t, claimReconciler(c))

	claim := getClaim(t, c)
	if claim.Status.Phase != localv1alpha1.ClaimPending {
		t.Errorf("phase = %q, want Pending", claim.Status.Phase)
	}
	cond := meta.FindStatusCondition(claim.Status.Conditions, localv1alpha1.ConditionClassResolved)
	if cond == nil || cond.Reason != localv1alpha1.ReasonClassNotFound {
		t.Errorf("ClassResolved condition = %+v, want reason ClassNotFound", cond)
	}
}

func TestBoundClaimGoesLostWhenAddressDisappears(t *testing.T) {
	claim := pendingClaim(testClassName)
	claim.Finalizers = []string{localv1alpha1.ClaimProtectionFinalizer}
	claim.Annotations = map[string]string{localv1alpha1.ProvisionerAnnotation: testProvisioner}
	claim.Status = localv1alpha1.IPAddressClaimStatus{
		Phase:     localv1alpha1.ClaimBound,
		ClassName: testClassName,
		Addresses: []localv1alpha1.BoundAddress{{Name: "ip-gone", Address: "203.0.113.13"}},
	}
	c := testClient(t, publicClass(nil), claim)
	reconcileClaim(t, claimReconciler(c))

	got := getClaim(t, c)
	if got.Status.Phase != localv1alpha1.ClaimLost {
		t.Errorf("phase = %q, want Lost", got.Status.Phase)
	}
}

func hasFinalizer(finalizers []string, want string) bool {
	for _, f := range finalizers {
		if f == want {
			return true
		}
	}
	return false
}
