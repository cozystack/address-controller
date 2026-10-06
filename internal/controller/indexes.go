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
	"net/netip"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
)

// IPAddressClaimRefIndex indexes IPAddress objects by the
// "<namespace>/<name>" of their spec.claimRef.
const IPAddressClaimRefIndex = "spec.claimRef"

// ClaimRefIndexKey builds the index key for a claim.
func ClaimRefIndexKey(namespace, name string) string {
	return namespace + "/" + name
}

// SetupIndexes registers the field indexes shared by the controllers. Call
// once, before setting up the controllers.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(ctx, &localv1alpha1.IPAddress{}, IPAddressClaimRefIndex,
		func(o client.Object) []string {
			addr := o.(*localv1alpha1.IPAddress)
			if addr.Spec.ClaimRef == nil {
				return nil
			}
			return []string{ClaimRefIndexKey(addr.Spec.ClaimRef.Namespace, addr.Spec.ClaimRef.Name)}
		})
}

// familyOf reports the address family of a textual IP, or "" if it does not
// parse.
func familyOf(address string) localv1alpha1.AddressFamily {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return ""
	}
	if ip.Is4() || ip.Is4In6() {
		return localv1alpha1.FamilyIPv4
	}
	return localv1alpha1.FamilyIPv6
}
