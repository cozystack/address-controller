# Operating address-controller

Procedures for cluster admins. The [design](design.md) explains why the
controller behaves as described here.

## Adopting an address a Service already holds

A Service pinned by hand to an address of a class's range keeps it while
it moves under the ledger, because the driver's pin equals the existing
one:

1. Create an `IPAddressClaim` in the Service's namespace that names the
   class and, in `spec.addressName`, the `IPAddress` of step 2. The MetalLB
   driver does not provision for a claim that names an address, so the
   claim waits `Pending` for that one. Where the optional `spec.addressName` admission
   policy is enabled, this takes an allowlisted principal.

   ```yaml
   apiVersion: local.sdn.cozystack.io/v1alpha1
   kind: IPAddressClaim
   metadata:
     name: web
     namespace: tenant-a
   spec:
     className: public
     addressName: ip-203-0-113-7
   ```

2. Create the `IPAddress` pre-bound to it:

   ```yaml
   apiVersion: local.sdn.cozystack.io/v1alpha1
   kind: IPAddress
   metadata:
     name: ip-203-0-113-7
   spec:
     address: 203.0.113.7
     className: public
     reclaimPolicy: Retain
     claimRef: {namespace: tenant-a, name: web}
     source: {fromClass: {}}
   ```

   The core accepts the pre-binding only if the class and family match the
   claim and the claim holds no address of that family yet; otherwise the
   claim records a `PreBindingRefused` event.
3. Annotate the Service with `local.sdn.cozystack.io/ip-address-claim: web`.
   The driver records the association and pins the same address.

The hand-written pin stays on the Service. Where metallb-iad's optional
admission policy is enabled, removing it later takes an allowlisted
principal. Without that policy, removing it is not gated, and only the
driver's conflict detection reports the address going to another Service.

Creating the `IPAddress` before the claim does not work: an address whose
`claimRef` names a missing claim is reclaimed at once, `Released` under
`Retain` and deleted under `Delete`.

## Removing the controllers

Claims, addresses and classes carry finalizers that the core removes
(`claim-protection`, `address-protection`, `class-protection`), and the
MetalLB driver adds `teardown` to the addresses it creates. While objects
remain and a controller is not running, deleting them, or a namespace that
holds claims, hangs. Tear down in this order:

1. Remove the claim annotation from Services, or delete the Services.
2. Delete every `IPAddressClaim`. The core reclaims each address by its
   `reclaimPolicy`.
3. Delete the `IPAddress` objects left behind (`Released`, `Available`).
   The driver releases each backend reservation.
4. Delete the `IPAddressClass` objects. Each goes once no address of it is
   left.
5. Uninstall the driver, then the core. `helm uninstall` leaves the CRDs.

If the controllers are already gone, clear the finalizers by hand, for
example:

```sh
kubectl get ipaddressclaims.local.sdn.cozystack.io -A \
  -o jsonpath='{range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}' |
  while read -r ns name; do
    kubectl -n "$ns" patch ipaddressclaims.local.sdn.cozystack.io "$name" \
      --type=merge -p '{"metadata":{"finalizers":null}}'
  done
for kind in ipaddresses ipaddressclasses; do
  kubectl get "$kind.local.sdn.cozystack.io" -o name |
    xargs -r -n1 kubectl patch --type=merge -p '{"metadata":{"finalizers":null}}'
done
```

The group suffix matters: `ipaddresses` alone resolves to the Kubernetes
`networking.k8s.io` kind. Backend state the driver would have released,
such as MetalLB placeholder Services, is then left for you to delete.

### Deleting a class

A class is held until no `IPAddress` of it is left, in any phase. Under
`reclaimPolicy: Retain` a deleted claim leaves its addresses `Released`,
and a class's `releaseDelay` returns them to `Available` rather than
deleting them, so a class that has served claims keeps addresses that hold
it. Delete those `IPAddress` objects, and the class goes. Until then the
class reports a `ClassInUse` event naming them.

## Quarantining an address

`ipaddress.local.sdn.cozystack.io/held: "true"` on a `Released` address
keeps its class's `releaseDelay` from returning it to the pool. The
annotation pauses the return, not the delay: removing it from an address
that has been `Released` for longer than `releaseDelay` returns the
address on the next pass.

## Upgrading the CRDs

The API is `v1alpha1` with no conversion webhook. Helm installs the chart's
`crds/` on first install only; `make install-crds` applies the current
CRDs, and a distribution that replaces CRDs on upgrade does the same. A
field removed or renamed in a later CRD invalidates stored objects that
still set it, so every release states its schema changes:

| release | schema change |
|---|---|
| v0.1.0 | initial `IPAddress`, `IPAddressClaim`, `IPAddressClass` |
| unreleased | `IPAddressClass.spec.releaseDelay` added; `IPAddress` gains a core-set `Bound` condition |
