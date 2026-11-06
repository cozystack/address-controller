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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

func reconcileClass(t *testing.T, c client.Client) {
	t.Helper()
	r := &IPAddressClassReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: testClassName},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// deletedClass returns a client holding objs and the test class, deleted
// while it carries the protection finalizer.
func deletedClass(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	class := publicClass(nil)
	class.Finalizers = []string{localv1alpha1.ClassProtectionFinalizer}
	c := testClient(t, append(objs, class)...)
	if err := c.Delete(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClassGetsProtectionFinalizer(t *testing.T) {
	c := testClient(t, publicClass(nil))
	reconcileClass(t, c)

	class := &localv1alpha1.IPAddressClass{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: testClassName}, class); err != nil {
		t.Fatal(err)
	}
	if !hasFinalizer(class.Finalizers, localv1alpha1.ClassProtectionFinalizer) {
		t.Errorf("finalizers = %v, want the class-protection finalizer", class.Finalizers)
	}
}

func TestDeletedClassWaitsForItsAddresses(t *testing.T) {
	// Any address of the class holds it, bound or not.
	for _, addr := range []*localv1alpha1.IPAddress{
		boundAddress("ip-bound", "203.0.113.30", localv1alpha1.IPAddressBound),
		boundAddress("ip-released", "203.0.113.31", localv1alpha1.IPAddressReleased),
	} {
		t.Run(addr.Name, func(t *testing.T) {
			c := deletedClass(t, addr)
			reconcileClass(t, c)

			if err := c.Get(context.Background(), types.NamespacedName{Name: testClassName}, &localv1alpha1.IPAddressClass{}); err != nil {
				t.Errorf("class gone while %s exists: %v", addr.Name, err)
			}
		})
	}
}

func TestDeletedClassIsReleasedWithoutAddresses(t *testing.T) {
	other := boundAddress("ip-other", "203.0.113.32", localv1alpha1.IPAddressBound)
	other.Spec.ClassName = otherClassName
	c := deletedClass(t, other)
	reconcileClass(t, c)

	err := c.Get(context.Background(), types.NamespacedName{Name: testClassName}, &localv1alpha1.IPAddressClass{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("class still present with no addresses of its own: %v", err)
	}
}

func TestDeletedClassIsNotChosenByDefault(t *testing.T) {
	class := defaultClass()
	class.Finalizers = []string{localv1alpha1.ClassProtectionFinalizer}
	c := testClient(t, class, pendingClaim(""))
	if err := c.Delete(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	reconcileClaim(t, claimReconciler(c))

	if got := getClaim(t, c); got.Status.ClassName != "" {
		t.Errorf("claim resolved to the deleted default class %q", got.Status.ClassName)
	}
}

func TestDeletedClassIsNotChosenForAutoClaim(t *testing.T) {
	class := autoClaimClass(testClassName)
	class.Finalizers = []string{localv1alpha1.ClassProtectionFinalizer}
	c := testClient(t, class, loadBalancerService())
	if err := c.Delete(context.Background(), class); err != nil {
		t.Fatal(err)
	}
	reconcileService(t, serviceReconciler(c))

	if claim := getAutoClaim(t, c); claim != nil {
		t.Errorf("claim auto-provisioned from the deleted class %q", claim.Spec.ClassName)
	}
}
