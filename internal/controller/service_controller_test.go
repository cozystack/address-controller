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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

const (
	testServiceName = "web-lb"
	testServiceUID  = "svc-uid-1"
	serviceKind     = "Service"
)

func serviceReconciler(c client.Client) *ServiceReconciler {
	return &ServiceReconciler{
		Client:   c,
		Scheme:   c.Scheme(),
		Recorder: record.NewFakeRecorder(100),
	}
}

func reconcileService(t *testing.T, r *ServiceReconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: testServiceName},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getService(t *testing.T, c client.Client) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testServiceName}, svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	return svc
}

// getAutoClaim returns the claim named after the test Service, or nil.
func getAutoClaim(t *testing.T, c client.Client) *localv1alpha1.IPAddressClaim {
	t.Helper()
	claim := &localv1alpha1.IPAddressClaim{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: testServiceName}, claim)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	return claim
}

func autoClaimClass(name string) *localv1alpha1.IPAddressClass {
	class := publicClass(map[string]string{localv1alpha1.IsAutoClaimClassAnnotation: "true"})
	class.Name = name
	return class
}

func loadBalancerService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testServiceName, Namespace: testNamespace, UID: testServiceUID},
		Spec: corev1.ServiceSpec{
			Type:       corev1.ServiceTypeLoadBalancer,
			IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol},
		},
	}
}

func TestAutoClaimCreatesOwnedClaimAndAnnotatesService(t *testing.T) {
	c := testClient(t, autoClaimClass(testClassName), loadBalancerService())
	reconcileService(t, serviceReconciler(c))

	claim := getAutoClaim(t, c)
	if claim == nil {
		t.Fatal("no claim auto-provisioned")
	}
	if claim.Spec.ClassName != testClassName {
		t.Errorf("claim className = %q, want %q", claim.Spec.ClassName, testClassName)
	}
	if claim.Spec.Family != localv1alpha1.FamilyIPv4 {
		t.Errorf("claim family = %q, want IPv4", claim.Spec.Family)
	}
	if ref := metav1.GetControllerOf(claim); ref == nil || ref.Kind != serviceKind || ref.UID != testServiceUID {
		t.Errorf("claim controller = %+v, want the Service", ref)
	}
	if got := getService(t, c).Annotations[localv1alpha1.ServiceClaimAnnotation]; got != testServiceName {
		t.Errorf("service claim annotation = %q, want %q", got, testServiceName)
	}
}

func TestAutoClaimSkipsIneligibleServices(t *testing.T) {
	cases := map[string]func(*corev1.Service){
		"not a LoadBalancer": func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeClusterIP },
		"names a claim": func(s *corev1.Service) {
			s.Annotations = map[string]string{localv1alpha1.ServiceClaimAnnotation: "mine"}
		},
		"opted out": func(s *corev1.Service) {
			s.Annotations = map[string]string{localv1alpha1.ServiceAutoClaimAnnotation: "false"}
		},
		"other load balancer class": func(s *corev1.Service) { s.Spec.LoadBalancerClass = new("example.com/lb") },
		"asks for a specific IP":    func(s *corev1.Service) { s.Spec.LoadBalancerIP = "203.0.113.50" },
		"names a backend pool": func(s *corev1.Service) {
			s.Annotations = map[string]string{"metallb.io/address-pool": "public"}
		},
		"pins through the legacy prefix": func(s *corev1.Service) {
			s.Annotations = map[string]string{"metallb.universe.tf/loadBalancerIPs": "203.0.113.52"}
		},
		"shares an address": func(s *corev1.Service) {
			s.Annotations = map[string]string{"metallb.io/allow-shared-ip": "web"}
		},
		"already addressed": func(s *corev1.Service) {
			s.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.51"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			svc := loadBalancerService()
			mutate(svc)
			c := testClient(t, autoClaimClass(testClassName), svc)
			reconcileService(t, serviceReconciler(c))

			if claim := getAutoClaim(t, c); claim != nil {
				t.Errorf("claim auto-provisioned for an ineligible Service")
			}
		})
	}
}

func TestAutoClaimWithoutMarkedClassLeavesServiceAlone(t *testing.T) {
	c := testClient(t, defaultClass(), loadBalancerService())
	reconcileService(t, serviceReconciler(c))

	if claim := getAutoClaim(t, c); claim != nil {
		t.Error("claim auto-provisioned although no class is marked for auto-claim")
	}
	if _, ok := getService(t, c).Annotations[localv1alpha1.ServiceClaimAnnotation]; ok {
		t.Error("service annotated although nothing was claimed")
	}
}

func TestAutoClaimWithMultipleMarkedClassesClaimsNothing(t *testing.T) {
	c := testClient(t, autoClaimClass("a"), autoClaimClass("b"), loadBalancerService())
	reconcileService(t, serviceReconciler(c))

	if claim := getAutoClaim(t, c); claim != nil {
		t.Error("claim auto-provisioned despite an ambiguous auto-claim class")
	}
}

func TestAutoClaimUsesClassNamedByService(t *testing.T) {
	svc := loadBalancerService()
	svc.Annotations = map[string]string{localv1alpha1.ServiceClassAnnotation: otherClassName}
	c := testClient(t, autoClaimClass(testClassName), svc)
	reconcileService(t, serviceReconciler(c))

	claim := getAutoClaim(t, c)
	if claim == nil {
		t.Fatal("no claim auto-provisioned")
	}
	if claim.Spec.ClassName != otherClassName {
		t.Errorf("claim className = %q, want the Service's %q", claim.Spec.ClassName, otherClassName)
	}
}

func TestAutoClaimRefusesForeignClaimOfSameName(t *testing.T) {
	foreign := pendingClaim(testClassName)
	foreign.Name = testServiceName
	c := testClient(t, autoClaimClass(testClassName), loadBalancerService(), foreign)
	reconcileService(t, serviceReconciler(c))

	if _, ok := getService(t, c).Annotations[localv1alpha1.ServiceClaimAnnotation]; ok {
		t.Error("service pointed at a claim it does not control")
	}
	if claim := getAutoClaim(t, c); metav1.GetControllerOf(claim) != nil {
		t.Error("foreign claim was adopted")
	}
}

func TestAutoClaimResumesWithOwnedClaim(t *testing.T) {
	// A crash between creating the claim and annotating the Service leaves
	// an owned claim behind; the next pass must finish the job.
	svc := loadBalancerService()
	owned := pendingClaim(testClassName)
	owned.Name = testServiceName
	owned.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: serviceKind, Name: testServiceName, UID: testServiceUID, Controller: new(true),
	}}
	c := testClient(t, autoClaimClass(testClassName), svc, owned)
	reconcileService(t, serviceReconciler(c))

	if got := getService(t, c).Annotations[localv1alpha1.ServiceClaimAnnotation]; got != testServiceName {
		t.Errorf("service claim annotation = %q, want %q", got, testServiceName)
	}
}

func TestFamilyForService(t *testing.T) {
	cases := map[string]struct {
		families []corev1.IPFamily
		policy   corev1.IPFamilyPolicy
		want     localv1alpha1.AddressFamily
	}{
		"single v4":        {[]corev1.IPFamily{corev1.IPv4Protocol}, corev1.IPFamilyPolicySingleStack, localv1alpha1.FamilyIPv4},
		"single v6":        {[]corev1.IPFamily{corev1.IPv6Protocol}, corev1.IPFamilyPolicySingleStack, localv1alpha1.FamilyIPv6},
		"require dual":     {[]corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol}, corev1.IPFamilyPolicyRequireDualStack, localv1alpha1.FamilyDual},
		"prefer dual":      {[]corev1.IPFamily{corev1.IPv6Protocol, corev1.IPv4Protocol}, corev1.IPFamilyPolicyPreferDualStack, localv1alpha1.FamilyIPv6},
		"families not set": {nil, "", localv1alpha1.FamilyIPv4},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc := loadBalancerService()
			svc.Spec.IPFamilies = tc.families
			if tc.policy != "" {
				svc.Spec.IPFamilyPolicy = new(tc.policy)
			}
			if got := familyForService(svc); got != tc.want {
				t.Errorf("family = %q, want %q", got, tc.want)
			}
		})
	}
}
