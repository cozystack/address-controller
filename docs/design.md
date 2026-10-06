# Design: the core address-binding controller

- **Component:** `address-controller` — the class-agnostic core of "IP
  addresses as a first-class resource"
  ([cozystack/community#35](https://github.com/cozystack/community/pull/35))
- **API group:** `local.sdn.cozystack.io/v1alpha1`
- **Status:** implemented (alpha)

This document records the contract: the resource model, the two state
machines the core drives, the exact reconciliation algorithms, and the
obligations a per-class driver must meet. It is the reference against which
drivers (such as [metallb-iad](https://github.com/cozystack/metallb-iad))
are written.

## 1. Scope

The core is the analog of the generic PVC/PV binding controller in the
storage subsystem. It owns:

- resolving a claim to a class,
- accepting and completing claim–address bindings,
- matching pre-provisioned addresses to claims,
- phase and condition bookkeeping on both sides,
- finalizer-driven reclaim when a claim is deleted.

It never allocates an address, never interprets class parameters, never
touches a Service, and never talks to a backend. Everything
backend-specific is a per-class driver's job, in a separate deployment,
discovered through the class's `spec.provisioner` — exactly as a
StorageClass names a CSI driver.

## 2. Resource model

| storage analog | kind | scope | one-line role |
|---|---|---|---|
| `StorageClass` | `IPAddressClass` | cluster | which pool, which driver, which reclaim policy |
| `PersistentVolume` | `IPAddress` | cluster | one concrete IP: the reservation and the inventory record |
| `PersistentVolumeClaim` | `IPAddressClaim` | namespaced | "give me one" — the whole tenant-facing API |

### IPAddressClass

- `spec.provisioner` (immutable) — the driver that fulfils claims of this
  class.
- `spec.reclaimPolicy` — `Retain` (default) or `Delete`; copied onto each
  provisioned `IPAddress`, where it takes effect. What each policy
  actually reclaims — the API object, the ledger entry, backend state —
  is spelled out in §6.
- `spec.parameters` — an opaque object (unknown fields preserved), never
  read by the core. Shape is defined by the driver.
- The annotation `ipaddressclass.local.sdn.cozystack.io/is-default-class:
  "true"` marks the class used by claims that name none.

### IPAddress

- `spec.address` (immutable) — the IP, one per object. The object **is**
  the reservation.

  *Why spec and not status:* the split here is request vs. record, not
  desired vs. observed. The **claim** is the request; an `IPAddress` is
  created only after an allocation is a fact — the driver performs or
  observes the backend operation first (carve from a range, allocate at
  a provider, adopt an existing reservation) and then records the
  result. The address is therefore always known at creation, and there
  is no observe-later phase for a status field to serve: an `IPAddress`
  is never a pending request for an address (that is what a `Pending`
  claim is), it is the ledger entry for one that exists.

  Given that, the field must provide *identity*, and identity cannot
  live in status. The object's name derives from the IP, one-object-
  per-IP is enforced by create-time collision on exactly this value, and
  conflict detection indexes it — all of which needs the value present
  from the first moment and immutable for the object's life. Status is
  the opposite by design: mutable, and legitimately absent at any moment
  (wiped status must be reconstructible from the world). For the
  `providerRef` arm the IP genuinely *is* re-observable — describe
  `eipalloc-…` and read it back — but for the `fromClass` arm the ledger
  entry is the only record in existence, and a union shares one schema:
  the arm with nothing to re-observe from decides the placement. The
  closest precedent is `Service.spec.clusterIP`: allocated by the
  system, not the user, yet spec — because it is an allocation result
  the system *commits to* and everything downstream references, not an
  observation that may drift. (Cluster API's IPAM contract records the
  address in spec likewise; KEP-1880 goes further and puts it in the
  object name.)

  Note what this does **not** imply: a provider where the IP cannot be
  *chosen* at allocation time — AWS hands you whatever `AllocateAddress`
  returns — is fully supported. Choosing at allocation is not the Pin
  capability; Pin is attaching a specific *already-reserved* address to
  a workload at association time, which such providers do natively
  (associate by the `eipalloc-…` handle). The driver allocates, observes
  the handle and the IP, and records both (`spec.address`,
  `spec.source.providerRef`). Only a backend with no pin mechanism at
  all — nothing that says "use this reserved address" at association
  time — is disqualified, because a reservation that can never be
  re-attached is not a reservation.
- `spec.className` — the class it belongs to.
- `spec.claimRef` `{namespace, name, uid}` — the binding. `uid` may be
  empty at creation (a driver pre-binding); the core completes it. This
  field is the *single authoritative record* of a binding; everything else
  (claim status, phases) is derived from it.
- `spec.reclaimPolicy` — what happens to this object when its claim goes.
- `spec.source` — a union, exactly one member set:
  - `fromClass: {}` — the driver carved it from the class's range; the
    driver is the IPAM of record;
  - `providerRef: {id}` — a reservation held by an external provider
    under the provider's own IAM, recorded here by its stable handle —
    whether the driver allocated it at the provider on demand or adopted
    one that already existed.
- `status.phase` — see the state machine below.
- `status.associatedTo` — the workload the address is currently announced
  for. `nil` means *reserved but inert*: held, attached to nothing.
  Written by drivers only.

### IPAddressClaim

- `spec.className` — empty means the default class.
- `spec.family` — `IPv4` | `IPv6` | `Dual`. `Dual` binds two addresses.
- `spec.addressName` — optional pre-binding to one specific `Available`
  address (the `PVC.spec.volumeName` analog); meaningful for single-family
  claims.
- `status.phase` — `Pending` | `Bound` | `Lost`.
- `status.className` — the sticky record of which class the claim resolved
  to (see §4 step 6).
- `status.addresses` — a list of `{name, address}`, one entry per bound
  `IPAddress`, so a `Dual` claim reports both families. This is what a
  tenant reads and puts in DNS.

## 3. Ownership: who writes what

The contract is largely a discipline about who may write which field.

| field | tenant | admin | core | driver |
|---|---|---|---|---|
| `Claim.spec` | ✍ creates/edits | | | |
| `Claim` provisioner annotation | | | ✍ stamps | reads |
| `Claim.status.*` | | | ✍ | never |
| `Claim` protection finalizer | | | ✍ | |
| `Address` object creation | | ✍ (static pre-provisioning) | never | ✍ (provisioning) |
| `Address.spec.claimRef` | | ✍ may clear on `Released` | ✍ sets on match / completes uid | ✍ pre-sets at creation only |
| `Address.status.phase`: `Available`/`Bound`/`Released` | | | ✍ | never |
| `Address.status.phase`: `Conflict`/`Lost` | | | never (sticky) | ✍ sets and clears |
| `Address.status.associatedTo` | | | never | ✍ |
| `Address` protection finalizer | | | ✍ | |
| `Address` driver finalizer(s) | | | | ✍ |
| `IPAddressClass` | | ✍ | reads | reads (incl. parameters) |

Two invariants fall out of this table:

1. **One binding record.** `Address.spec.claimRef` is the only place a
   binding lives. `Claim.status.addresses` is a projection of it, always
   recomputed, never authoritative.
2. **Phase partition.** `Available`, `Bound`, `Released` belong to the
   core; `Conflict` and `Lost` belong to drivers, and the core treats them
   as sticky — it neither enters nor leaves them. This is what lets a
   driver flag backend-level facts (a collision, a lost provider
   reservation) without racing the core's bookkeeping.

## 4. The claim reconciliation algorithm

State machine:

```
            class resolved &        all requested families
            no address yet          bound & not Lost
  (new) ──────► Pending ─────────────────► Bound
                   ▲                        │
                   │   (never: Lost is      │  a bound address
                   │    not re-entered      ▼  disappears / goes Lost
                   │    from Pending)      Lost
                   └────────────────────────┘
                        all families satisfied again
```

Each reconcile pass runs the following sequence. Every step is
idempotent; the pass re-derives everything from the cluster state.

1. **Deletion?** If the claim is being deleted, run the *reclaim flow*
   (§6) and stop.
2. **Protection.** Ensure the claim carries the
   `local.sdn.cozystack.io/claim-protection` finalizer, so deletion always
   passes through the reclaim flow.
3. **Collect bound addresses.** All `IPAddress` objects whose `claimRef`
   names this claim's namespace/name, and whose `claimRef.uid` is either
   empty or equal to the claim's UID. A UID *mismatch* means the address
   is bound to an earlier, deleted claim that happened to have the same
   name — a stale binding. It is never adopted; the address reconciler
   reclaims it (§5 step 6).
4. **Complete pre-bindings.** For collected addresses with an empty
   `claimRef.uid` (a driver pre-bound them at creation), write the claim's
   UID. This is the core's acceptance of the driver's provisioning.
5. **Anything missing?** Expand `spec.family` into concrete families
   (`Dual` → v4 + v6); a family is satisfied if some collected address of
   that family exists and is not in phase `Lost`. **If every family is
   satisfied, skip straight to status (step 8) — the class is not
   consulted at all.** Binding state comes strictly before class state:
   a fully bound claim needs nothing from its class, so a deleted class
   never disturbs existing bindings (see *Class deletion* below).
6. **Class resolution** (only reached with families missing). Resolve a
   class name, trying in order:
   1. `spec.className`, if set (an explicit spec always wins);
   2. `status.className` — the sticky record of a previous resolution, so
      a claim that resolved against a default class does not flap when the
      default annotation moves;
   3. the *default class*: the exactly-one `IPAddressClass` annotated as
      default. Zero or more than one is a terminal condition for this pass
      (`NoDefaultClass` / `MultipleDefaultClasses` on the `ClassResolved`
      condition); the claim is retried when any class changes. The core
      deliberately refuses to guess among multiple defaults.

   The named class must exist (`ClassNotFound` condition otherwise; the
   pass still falls through to step 8 so phase and addresses stay
   maintained). On success the core **stamps** the claim with the
   annotation `local.sdn.cozystack.io/provisioner:
   <class.spec.provisioner>`. The stamp always mirrors the resolved
   class — if a claim is re-targeted at a different class before binding,
   the stamp follows. This annotation is the driver's watch key; it is
   how a driver knows a claim is its to serve without ever resolving
   classes itself.
7. **Match, per missing family.** For each unsatisfied family:
   - candidate set: if `spec.addressName` is set, only that object;
     otherwise every address in phase `Available` (so: no `claimRef`, and
     already accepted by the address reconciler) with the resolved class
     and the wanted family;
   - bind the candidate with the lexicographically smallest name by
     writing `claimRef {namespace, name, uid}`. Determinism makes
     concurrent controllers converge; optimistic concurrency resolves
     races (the loser's write fails, it re-lists and takes the next
     candidate);
   - no candidate → the family stays unsatisfied and the claim waits for
     its driver to provision.
8. **Status.** Recompute from scratch:
   - all families satisfied → `Bound`;
   - previously `Bound` (or `Lost`) and now unsatisfied → `Lost` — a
     binding degraded, which is surfaced, never silently re-provisioned
     around; the claim returns to `Bound` by itself if the address comes
     back;
   - otherwise `Pending`, with the `Bound` condition explaining what it
     waits for (`WaitingForProvisioning` names the stamped provisioner).
   `status.addresses` lists every collected address (name + IP), sorted
   by object name. `status.className` records the resolution when one
   happened this pass and is left untouched otherwise.

**Class deletion.** The class is load-bearing only at two moments:
resolving/stamping an unbound claim, and matching or provisioning *new*
bindings. Everything downstream deliberately avoids depending on it:
satisfied claims skip class resolution entirely (step 5), reclaim reads
the `reclaimPolicy` **copied onto each address** at provisioning time
(§6), and the address reconciler never reads classes at all. So deleting
an `IPAddressClass` — or restarting the controller after its deletion —
leaves every existing binding, every reclaim, and all status upkeep
intact; the only effect is that still-unbound claims of that class stop
progressing, honestly reported as `Pending` with `ClassNotFound`.

## 5. The address reconciliation algorithm

State machine (core-owned transitions solid, driver-owned dashed):

```
   (new) ──► Pending ──► Available ◄────────────────┐
   (invalid     │            │  ▲                   │ admin clears
    address     │       core matches / driver       │ claimRef
    stays       │       pre-binds + core accepts    │
    Pending)    │            ▼  │                   │
                │          Bound ───────────────► Released
                │            ┆      claim deleted     │
                │            ┆      (Retain)          │ (Delete: object
                │            ┆                        │  is deleted instead)
                │            ▼┄┄┄┄ driver-owned ┄┄┄┄┄▼
                └──────►  Conflict / Lost   (sticky for the core;
                          set and cleared by the driver only)
```

Each pass:

1. **Deletion?** The protection finalizer
   (`local.sdn.cozystack.io/address-protection`) is released only when no
   live claim holds the address — i.e. the address is not `Bound`, or the
   referenced claim is missing, being deleted, or has a different UID.
   Otherwise deletion blocks and waits: a `Bound` address cannot vanish
   from under a live claim. (Driver finalizers are independent of this
   and are the driver's own business.)
2. **Protection.** Ensure the finalizer.
3. **Validation.** An unparsable `spec.address` pins the object at
   `Pending`; nothing downstream trusts an invalid IP.
4. **Sticky phases.** `Conflict` and `Lost` short-circuit the pass —
   driver territory (§3).
5. **Unbound.** No `claimRef` → `Available`. This single rule is also the
   recycling path: an admin clears the `claimRef` of a `Released` address
   and it becomes matchable again — the deliberate, manual step PV
   semantics prescribe for `Retain`.
6. **Bound.** With a `claimRef`:
   - claim exists, UID matches → `Bound`;
   - claim exists, `claimRef.uid` empty → wait (the claim reconciler is
     about to complete the binding);
   - claim is being deleted → wait (the claim-side reclaim flow owns the
     transition);
   - claim gone, or UID mismatch → **safety-net reclaim**: apply this
     address's own `reclaimPolicy` (`Delete` → delete self, `Retain` →
     `Released`). This duplicates the claim-side flow on purpose: reclaim
     must happen even if the claim vanished without its finalizer running
     (e.g. the finalizer was force-removed).

## 6. The reclaim flow (claim deletion)

Runs behind the claim's protection finalizer, so it always runs before
the claim disappears:

- For every address bound to the claim (same collection rule as §4 step
  3), apply **the address's** `reclaimPolicy` — copied from the class at
  provisioning time, so a later class edit does not retroactively change
  the fate of existing addresses:
  - `Retain` → phase `Released`. The `claimRef` **survives** — the
    released address is not reusable until an admin clears it (§5 step 5).
  - `Delete` → delete the `IPAddress` object. The core does not tear down
    any backend state; the driver's own finalizer on the address
    intercepts the deletion and deallocates first (§7 obligation 4).
- Remove the finalizer; the claim goes away.

**What each policy actually reclaims.** "The address" is up to three
things: the API object, the reservation it records, and whatever backend
state stands behind it. The policies act on them differently:

- `Retain` acts on nothing but the phase. The object stays, so the
  reservation stays: a range-carved (`fromClass`) IP remains excluded from
  allocation, and a provider-held (`providerRef`) reservation remains held
  at the provider — **which means it keeps incurring provider charges**.
  That is deliberate, not a leak: retained means still yours, and an
  idle-but-billed address is exactly what holding an elastic IP is. The
  cost stops only when the reservation is actually given up, i.e. when
  someone deletes the `IPAddress` object.
- `Delete` removes the object, and the driver's teardown finalizer is
  where backend state is released before it goes: a `fromClass` address
  returns to the free range simply by its ledger entry ceasing to exist
  (for backends like MetalLB there is nothing else to do); a
  provider-side reservation **the driver itself created** must be
  released at the provider. For a reservation the driver merely *adopted*
  (it existed before the object, e.g. an admin-imported EIP), whether
  teardown releases it or leaves it in the provider's hands is the
  driver's documented policy call — the safe default is to leave what
  you did not create.

So the EIP question has a precise answer: `Retain` keeps the EIP and its
bill until an admin deletes the object; `Delete` releases a
driver-allocated EIP at the provider, via the driver's finalizer.

## 7. The driver contract

A per-class driver (the CSI-driver analog) plugs in with no registration
step: deploying it and creating an `IPAddressClass` naming it is the whole
integration. Its obligations:

1. **Serve stamped claims.** Watch `IPAddressClaim`s whose
   `local.sdn.cozystack.io/provisioner` annotation equals the driver's
   name and which are not fully bound. The driver must not resolve
   classes or defaults itself — the stamp is the assignment.
2. **Provision pre-bound.** For each family the claim still misses,
   create one `IPAddress`:
   - `spec.claimRef` pre-set to the claim's namespace/name (UID optional —
     the core completes it; setting it is allowed and slightly tighter);
   - `spec.className` set, `spec.reclaimPolicy` copied from the class;
   - `spec.source` reflecting reality: `fromClass` if the driver carved
     the address from the class's own range, `providerRef` if a provider
     holds the reservation (whether the driver allocated it there or
     adopted a pre-existing one);
   - the driver's **own finalizer**, if teardown has any backend work.
   The driver determines the IP — by choosing it from a range, or by
   recording what its backend handed out; the core decides *whether the
   binding stands* (it is the binder).
3. **Never touch core-owned state** (§3): claim status, the core phases,
   core finalizers, or the `claimRef` of any existing address.
4. **Tear down on deletion.** When an `IPAddress` the driver created is
   deleted (reclaim `Delete`, or an admin action), the driver's finalizer
   must release backend state before letting the object go.
5. **Own association** (optional but standard). Attaching a bound address
   to a workload is a separate, reversible act, entirely driver-side:
   resolve the Service annotation
   `local.sdn.cozystack.io/ip-address-claim` (naming a claim in the
   Service's *own* namespace — cross-namespace sharing is not a thing),
   translate it into the backend's pin mechanism, maintain
   `status.associatedTo`, and enforce that one claim serves one workload
   at a time. Disassociation must leave the address `Bound` — reserved,
   inert.
6. **Detect, and clear, conflicts.** If the backend hands the reserved
   address to a workload the binding does not authorize, set phase
   `Conflict` (and `Lost` if the backing reservation disappears). The
   core will never override these; the driver clears them by setting the
   phase back to a core-owned value once the condition has passed.

Static pre-provisioning needs no driver at all: an admin may create
`Available` addresses by hand and the core matches them (§4 step 7).

## 8. Interaction walkthrough (the happy path)

```
tenant                core                          driver
  │ create Claim        │                             │
  │────────────────────►│ finalizer, resolve class,   │
  │                     │ stamp provisioner ─────────►│ sees stamped, unbound claim
  │                     │                             │ allocates, creates IPAddress
  │                     │◄────────────────────────────│   (claimRef pre-set, own finalizer)
  │                     │ completes uid, phase Bound  │
  │◄────────────────────│ status.addresses = [ip]     │
  │ annotate Service    │                             │
  │──────────────────────────────────────────────────►│ resolves claim → ip,
  │                     │                             │ writes backend pin,
  │                     │                             │ sets associatedTo
  │ delete Service      │                             │ withdraws pin, clears associatedTo
  │                     │        (address stays Bound: reserved, inert)
  │ annotate Service B  │                             │ same ip, pinned to B
```

Deleting the claim, not the Service, is what releases the address — via
§6, honoring the reclaim policy. This asymmetry is the entire point of
the model.

## 9. Relation to the design proposal

Where [community#35](https://github.com/cozystack/community/pull/35) is
explicit, this implementation follows it. Where it is deliberately a stub,
the choices made here are: PV-style annotation stamping instead of a
driver-registration CRD; no modeling of driver capabilities
(Allocate/Adopt/Pin) until a registration object exists to declare them
on; opaque-object class parameters rather than a string map; refusal over
newest-wins for multiple default classes; dual-stack as two `IPAddress`
objects under one claim; sticky class resolution recorded in status
(there is no admission webhook to default the spec); and association,
Service-watching, and admission policy left entirely to drivers. The API
group is `local.sdn.cozystack.io` rather than the proposal's sketched
`ipam.cozystack.io`.
