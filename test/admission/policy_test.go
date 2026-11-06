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

// Package admission runs the chart's optional ValidatingAdmissionPolicy
// against a real kube-apiserver (envtest).
package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

const (
	tenantNS  = "tenant-a"
	otherNS   = "system-x"
	tenant    = "alice"
	machinery = "system:serviceaccount:billing:broker"
)

func TestAddressNamePolicy(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run through `make test`")
	}
	policy := renderPolicy(t)

	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../chart/address-controller/crds"}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := localv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin := newClient(t, cfg, scheme, "", nil)
	for _, obj := range fixtures() {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("creating fixture %s: %v", obj.GetName(), err)
		}
	}
	for _, obj := range policy {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("creating %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}
	alice := newClient(t, cfg, scheme, tenant, []string{"system:authenticated"})
	broker := newClient(t, cfg, scheme, machinery, []string{"system:serviceaccounts", "system:authenticated"})
	waitForEnforcement(t, alice)

	// pinned is a claim the broker bound to a specific address.
	pinned := claim("pinned", tenantNS, "ip-203-0-113-7")
	if err := admin.Create(ctx, pinned); err != nil {
		t.Fatal(err)
	}

	relabel := map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "web"}}}
	cases := []struct {
		name  string
		as    client.Client
		write func(client.Client) error
		admit bool
	}{
		{"tenant creates a claim", alice, createClaim(tenantNS, ""), true},
		{"tenant names an address", alice, createClaim(tenantNS, "ip-203-0-113-8"), false},
		{"tenant names an address outside the bound namespaces", alice, createClaim(otherNS, "ip-203-0-113-8"), true},
		{"tenant relabels a claim, addressName untouched", alice, patchPinned(relabel), true},
		{"tenant changes addressName", alice, patchPinned(addressName("ip-203-0-113-9")), false},
		{"tenant removes addressName", alice, patchPinned(addressName(nil)), false},
		{"allowlisted machinery names an address", broker, createClaim(tenantNS, "ip-203-0-113-8"), true},
		{"admin names an address", admin, createClaim(tenantNS, "ip-203-0-113-8"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.write(tc.as)
			switch {
			case tc.admit && err != nil:
				t.Errorf("denied, want admitted: %v", err)
			case !tc.admit && err == nil:
				t.Error("admitted, want denied")
			case !tc.admit && !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err):
				t.Errorf("failed for another reason than admission: %v", err)
			}
		})
	}
}

// renderPolicy renders the chart's admission policy, bound to namespaces
// labelled tenant=yes, with the broker allowlisted.
func renderPolicy(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	out, err := exec.Command("helm", "template", "address-controller", "../../chart/address-controller",
		"--set", "admissionPolicy.enabled=true",
		"--set-json", `admissionPolicy.namespaceSelector={"matchLabels":{"tenant":"yes"}}`,
		"--set-json", `admissionPolicy.allowedUsernames=["`+machinery+`"]`,
		"--show-only", "templates/admission-policy.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	var objs []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return objs
			}
			t.Fatalf("decoding rendered policy: %v", err)
		}
		if len(obj.Object) > 0 {
			objs = append(objs, obj)
		}
	}
}

// fixtures are the namespaces and the RBAC that lets the tenant and the
// broker write claims, so only admission can refuse them.
func fixtures() []client.Object {
	return []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS, Labels: map[string]string{"tenant": "yes"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: otherNS}},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "writers"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{
				{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: tenant},
				{Kind: rbacv1.ServiceAccountKind, Name: "broker", Namespace: "billing"},
			},
		},
	}
}

// waitForEnforcement waits until the API server has loaded the policy and
// its binding, which it does asynchronously.
func waitForEnforcement(t *testing.T, tenantClient client.Client) {
	t.Helper()
	probe := createClaim(tenantNS, "ip-203-0-113-1")
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := probe(tenantClient)
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy not enforced after 30s; last result: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func newClient(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme, user string, groups []string) client.Client {
	t.Helper()
	cfg = rest.CopyConfig(cfg)
	if user != "" {
		cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func claim(name, namespace, addressName string) *localv1alpha1.IPAddressClaim {
	return &localv1alpha1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       localv1alpha1.IPAddressClaimSpec{ClassName: "public", AddressName: addressName},
	}
}

// createClaim dry-runs the creation of a claim; admission runs in full on a
// dry run.
func createClaim(namespace, addressName string) func(client.Client) error {
	return func(c client.Client) error {
		cl := claim("", namespace, addressName)
		cl.GenerateName = "probe-"
		return c.Create(context.Background(), cl, client.DryRunAll)
	}
}

// patchPinned dry-runs a merge patch on the pinned claim.
func patchPinned(patch map[string]any) func(client.Client) error {
	return func(c client.Client) error {
		cl := &localv1alpha1.IPAddressClaim{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: tenantNS, Name: "pinned"}, cl); err != nil {
			return err
		}
		raw, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		return c.Patch(context.Background(), cl, client.RawPatch(types.MergePatchType, raw), client.DryRunAll)
	}
}

// addressName builds a merge patch that sets spec.addressName, or removes
// it when value is nil.
func addressName(value any) map[string]any {
	return map[string]any{"spec": map[string]any{"addressName": value}}
}
