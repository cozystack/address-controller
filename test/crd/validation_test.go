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

// Package crd runs the chart's CRD validation rules against a real
// kube-apiserver (envtest).
package crd

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

// The two classes the update cases patch.
const (
	withoutLBClass = "unset"
	withLBClass    = "set"
)

func TestIPAddressClassLoadBalancerClass(t *testing.T) {
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

	for name, value := range map[string]*string{withoutLBClass: nil, withLBClass: new("example.com/lb")} {
		if err := c.Create(ctx, class(name, value)); err != nil {
			t.Fatalf("creating class %s: %v", name, err)
		}
	}

	creates := []struct {
		name    string
		lbClass *string
		admit   bool
	}{
		{"create without loadBalancerClass", nil, true},
		{"create with loadBalancerClass", new("example.com/lb"), true},
		{"create with empty loadBalancerClass", new(""), false},
	}
	for _, tc := range creates {
		t.Run(tc.name, func(t *testing.T) {
			obj := class("", tc.lbClass)
			obj.GenerateName = "probe-"
			check(t, c.Create(ctx, obj, client.DryRunAll), tc.admit)
		})
	}

	updates := []struct {
		name  string
		class string
		spec  map[string]any
		admit bool
	}{
		{"set on a class created without it", withoutLBClass, lbClassPatch("example.com/lb"), false},
		{"other field changed, still unset", withoutLBClass, reclaimDelete, true},
		{"changed to another value", withLBClass, lbClassPatch("example.com/other"), false},
		{"removed", withLBClass, lbClassPatch(nil), false},
		{"rewritten to the same value", withLBClass, lbClassPatch("example.com/lb"), true},
		{"other field changed, value kept", withLBClass, reclaimDelete, true},
	}
	for _, tc := range updates {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"spec": tc.spec})
			if err != nil {
				t.Fatal(err)
			}
			obj := &localv1alpha1.IPAddressClass{}
			if err := c.Get(ctx, types.NamespacedName{Name: tc.class}, obj); err != nil {
				t.Fatal(err)
			}
			check(t, c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw), client.DryRunAll), tc.admit)
		})
	}
}

var reclaimDelete = map[string]any{"reclaimPolicy": "Delete"}

// lbClassPatch is a merge patch of spec setting loadBalancerClass, or removing
// it when value is nil.
func lbClassPatch(value any) map[string]any {
	return map[string]any{"loadBalancerClass": value}
}

func check(t *testing.T, err error, admit bool) {
	t.Helper()
	switch {
	case admit && err != nil:
		t.Errorf("refused, want admitted: %v", err)
	case !admit && err == nil:
		t.Error("admitted, want refused")
	case !admit && !apierrors.IsInvalid(err):
		t.Errorf("refused for another reason than validation: %v", err)
	}
}

func class(name string, loadBalancerClass *string) *localv1alpha1.IPAddressClass {
	return &localv1alpha1.IPAddressClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: localv1alpha1.IPAddressClassSpec{
			Provisioner:       "example.com/driver",
			ReclaimPolicy:     localv1alpha1.ReclaimRetain,
			LoadBalancerClass: loadBalancerClass,
		},
	}
}
