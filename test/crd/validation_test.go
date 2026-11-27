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

// Package crd runs the chart's CRDs against a real kube-apiserver (envtest).
package crd

import (
	"context"
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

// TestIPAddressClassStatusSubresource checks that the provisioner's status
// survives writes to the class itself: the core controller updates a class to
// manage its finalizer, and such an update must not reset what the
// provisioner reported.
func TestIPAddressClassStatusSubresource(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run through `make test`")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../chart/address-controller/crds"}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := localv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	class := &localv1alpha1.IPAddressClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public"},
		Spec: localv1alpha1.IPAddressClassSpec{
			Provisioner:   "example.com/driver",
			ReclaimPolicy: localv1alpha1.ReclaimRetain,
		},
	}
	if err := c.Create(ctx, class); err != nil {
		t.Fatalf("creating class: %v", err)
	}
	class.Status = localv1alpha1.IPAddressClassStatus{
		LoadBalancerClass: "example.com/lb",
		Conditions: []metav1.Condition{{
			Type:               localv1alpha1.ConditionAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             localv1alpha1.ReasonAccepted,
			ObservedGeneration: class.Generation,
			LastTransitionTime: metav1.Now(),
		}},
	}
	if err := c.Status().Update(ctx, class); err != nil {
		t.Fatalf("writing status: %v", err)
	}

	class.Finalizers = []string{"example.com/finalizer"}
	class.Status = localv1alpha1.IPAddressClassStatus{}
	if err := c.Update(ctx, class); err != nil {
		t.Fatalf("updating class: %v", err)
	}

	got := &localv1alpha1.IPAddressClass{}
	if err := c.Get(ctx, types.NamespacedName{Name: class.Name}, got); err != nil {
		t.Fatal(err)
	}
	lbClass, ok := got.AcceptedLoadBalancerClass()
	if !ok || lbClass != "example.com/lb" {
		t.Errorf("after an update of the class, AcceptedLoadBalancerClass() = %q, %t; want %q, true",
			lbClass, ok, "example.com/lb")
	}

	// Clear the finalizer so the environment can stop cleanly.
	got.Finalizers = nil
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
}
