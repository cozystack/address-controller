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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

var releasedAt = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// delayedClass is the test class with a one-hour releaseDelay.
func delayedClass() *localv1alpha1.IPAddressClass {
	class := publicClass(nil)
	class.Spec.ReleaseDelay = &metav1.Duration{Duration: time.Hour}
	return class
}

// releasedAddress is an address whose claim is gone, Released at releasedAt.
func releasedAddress() *localv1alpha1.IPAddress {
	addr := boundAddress("ip-released", "203.0.113.40", localv1alpha1.IPAddressReleased)
	addr.Spec.ClaimRef.UID = "deleted-claim-uid"
	addr.Status.Conditions = []metav1.Condition{{
		Type:               localv1alpha1.ConditionBound,
		Status:             metav1.ConditionFalse,
		Reason:             localv1alpha1.ReasonReleased,
		LastTransitionTime: metav1.NewTime(releasedAt),
	}}
	return addr
}

func reconcileAddressAt(t *testing.T, r *IPAddressReconciler, now time.Time) ctrl.Result {
	t.Helper()
	r.Now = func() time.Time { return now }
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ip-released"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func TestReleasedAddressReturnsAfterDelay(t *testing.T) {
	c := testClient(t, delayedClass(), releasedAddress())
	r := addressReconciler(c)
	reconcileAddressAt(t, r, releasedAt.Add(2*time.Hour))
	reconcileAddressAt(t, r, releasedAt.Add(2*time.Hour))

	got := getAddress(t, c, "ip-released")
	if got.Spec.ClaimRef != nil {
		t.Errorf("claimRef = %+v, want cleared after the delay", got.Spec.ClaimRef)
	}
	if got.Status.Phase != localv1alpha1.IPAddressAvailable {
		t.Errorf("phase = %q, want Available", got.Status.Phase)
	}
}

func TestReleasedAddressWaitsOutTheDelay(t *testing.T) {
	c := testClient(t, delayedClass(), releasedAddress())
	res := reconcileAddressAt(t, addressReconciler(c), releasedAt.Add(10*time.Minute))

	if got := getAddress(t, c, "ip-released"); got.Spec.ClaimRef == nil {
		t.Error("claimRef cleared before the delay elapsed")
	}
	if res.RequeueAfter != 50*time.Minute {
		t.Errorf("RequeueAfter = %s, want the remaining 50m", res.RequeueAfter)
	}
}

func TestReleasedAddressStaysWithoutDelayOrWhenHeld(t *testing.T) {
	held := releasedAddress()
	held.Annotations = map[string]string{localv1alpha1.HeldAnnotation: localv1alpha1.AnnotationTrue}
	cases := map[string]struct {
		class *localv1alpha1.IPAddressClass
		addr  *localv1alpha1.IPAddress
	}{
		"class has no delay": {publicClass(nil), releasedAddress()},
		"address is held":    {delayedClass(), held},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := testClient(t, tc.class, tc.addr)
			reconcileAddressAt(t, addressReconciler(c), releasedAt.Add(48*time.Hour))

			if got := getAddress(t, c, "ip-released"); got.Spec.ClaimRef == nil || got.Status.Phase != localv1alpha1.IPAddressReleased {
				t.Errorf("phase %q, claimRef %+v; want it kept Released", got.Status.Phase, got.Spec.ClaimRef)
			}
		})
	}
}

func TestPhaseIsDatedByTheBoundCondition(t *testing.T) {
	addr := unboundAddress("ip-41", "203.0.113.41")
	c := testClient(t, addr)
	reconcileAddress(t, addressReconciler(c), "ip-41")

	got := getAddress(t, c, "ip-41")
	cond := meta.FindStatusCondition(got.Status.Conditions, localv1alpha1.ConditionBound)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != localv1alpha1.ReasonAvailable {
		t.Errorf("Bound condition = %+v, want False/Available", cond)
	}
}
