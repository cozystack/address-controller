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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

func TestInventoryCollectorCountsByPhase(t *testing.T) {
	bound := boundAddress("ip-a", "203.0.113.50", localv1alpha1.IPAddressBound)
	conflict := boundAddress("ip-b", "203.0.113.51", localv1alpha1.IPAddressConflict)
	conflict2 := boundAddress("ip-c", "203.0.113.52", localv1alpha1.IPAddressConflict)
	claim := pendingClaim(testClassName)
	claim.Status.Phase = localv1alpha1.ClaimBound
	claim.Status.ClassName = testClassName
	c := testClient(t, bound, conflict, conflict2, claim)

	want := `
# HELP address_controller_ipaddresses IPAddress objects by class and phase.
# TYPE address_controller_ipaddresses gauge
address_controller_ipaddresses{class="public",phase="Bound"} 1
address_controller_ipaddresses{class="public",phase="Conflict"} 2
# HELP address_controller_ipaddressclaims IPAddressClaim objects by namespace, resolved class and phase.
# TYPE address_controller_ipaddressclaims gauge
address_controller_ipaddressclaims{class="public",namespace="tenant-a",phase="Bound"} 1
`
	if err := testutil.CollectAndCompare(&InventoryCollector{Reader: c}, strings.NewReader(want)); err != nil {
		t.Error(err)
	}
}
