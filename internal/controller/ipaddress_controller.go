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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

// IPAddressReconciler owns the class-agnostic address lifecycle: deletion
// protection while a live claim holds the address, phase bookkeeping between
// Available/Bound/Released, and the safety-net reclaim when a bound claim
// vanished without the claim-side flow running. The driver-owned phases
// Conflict and Lost are treated as sticky and never overwritten.
type IPAddressReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses/finalizers,verbs=update
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives an IPAddress's phase bookkeeping.
func (r *IPAddressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	addr := &localv1alpha1.IPAddress{}
	if err := r.Get(ctx, req.NamespacedName, addr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !addr.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalizeAddress(ctx, addr)
	}

	if !controllerutil.ContainsFinalizer(addr, localv1alpha1.AddressProtectionFinalizer) {
		controllerutil.AddFinalizer(addr, localv1alpha1.AddressProtectionFinalizer)
		if err := r.Update(ctx, addr); err != nil {
			return ctrl.Result{}, err
		}
	}

	if familyOf(addr.Spec.Address) == "" {
		r.Recorder.Eventf(addr, "Warning", "InvalidAddress",
			"spec.address %q does not parse as an IP address", addr.Spec.Address)
		return ctrl.Result{}, r.setPhase(ctx, addr, localv1alpha1.IPAddressPending)
	}

	// Conflict and Lost belong to the driver; the core controller neither
	// enters nor leaves them.
	if addr.Status.Phase == localv1alpha1.IPAddressConflict || addr.Status.Phase == localv1alpha1.IPAddressLost {
		return ctrl.Result{}, nil
	}

	if addr.Spec.ClaimRef == nil {
		// Covers a fresh unbound address and a Released one whose claimRef
		// an admin cleared: both become Available.
		return ctrl.Result{}, r.setPhase(ctx, addr, localv1alpha1.IPAddressAvailable)
	}

	claim, err := r.boundClaim(ctx, addr)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case claim == nil:
		// The claim is gone: reclaim it, unless the claim-side flow already
		// did (a stale UID, or a claim that vanished without its
		// finalizer), and then count down a Released address's delay.
		if err := r.reclaim(ctx, addr); err != nil {
			return ctrl.Result{}, err
		}
		return r.releaseAfterDelay(ctx, addr)
	case !claim.DeletionTimestamp.IsZero():
		// The claim-side flow owns the transition; nothing to do here.
		return ctrl.Result{}, nil
	case addr.Spec.ClaimRef.UID == "":
		// Driver pre-binding awaiting completion by the claim controller.
		return ctrl.Result{}, nil
	default:
		if err := r.setPhase(ctx, addr, localv1alpha1.IPAddressBound); err != nil {
			return ctrl.Result{}, err
		}
	}

	logger.V(1).Info("reconciled address", "phase", addr.Status.Phase)
	return ctrl.Result{}, nil
}

// finalizeAddress blocks deletion while a live, non-deleting claim still
// holds the address; otherwise it drops the protection finalizer.
func (r *IPAddressReconciler) finalizeAddress(ctx context.Context, addr *localv1alpha1.IPAddress) error {
	if !controllerutil.ContainsFinalizer(addr, localv1alpha1.AddressProtectionFinalizer) {
		return nil
	}
	if addr.Spec.ClaimRef != nil && addr.Status.Phase == localv1alpha1.IPAddressBound {
		claim, err := r.boundClaim(ctx, addr)
		if err != nil {
			return err
		}
		if claim != nil && claim.DeletionTimestamp.IsZero() {
			r.Recorder.Eventf(addr, "Warning", "DeletionBlocked",
				"IPAddress is bound to live claim %s/%s; deletion waits until the claim releases it",
				claim.Namespace, claim.Name)
			return nil
		}
	}
	controllerutil.RemoveFinalizer(addr, localv1alpha1.AddressProtectionFinalizer)
	return r.Update(ctx, addr)
}

// boundClaim fetches the claim named by claimRef. It returns nil for a
// missing claim and for a UID mismatch — both mean the binding is stale.
func (r *IPAddressReconciler) boundClaim(ctx context.Context, addr *localv1alpha1.IPAddress) (*localv1alpha1.IPAddressClaim, error) {
	ref := addr.Spec.ClaimRef
	claim := &localv1alpha1.IPAddressClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, claim)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ref.UID != "" && claim.UID != ref.UID {
		return nil, nil
	}
	return claim, nil
}

// reclaim applies the address's reclaim policy after its claim vanished.
func (r *IPAddressReconciler) reclaim(ctx context.Context, addr *localv1alpha1.IPAddress) error {
	if addr.Spec.ReclaimPolicy == localv1alpha1.ReclaimDelete {
		r.Recorder.Event(addr, "Normal", "Reclaimed", "claim gone; deleting per reclaimPolicy Delete")
		if err := r.Delete(ctx, addr); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	if addr.Status.Phase != localv1alpha1.IPAddressReleased {
		r.Recorder.Event(addr, "Normal", "Reclaimed", "claim gone; released per reclaimPolicy Retain")
	}
	return r.setPhase(ctx, addr, localv1alpha1.IPAddressReleased)
}

func (r *IPAddressReconciler) setPhase(ctx context.Context, addr *localv1alpha1.IPAddress, phase localv1alpha1.IPAddressPhase) error {
	if !markPhase(addr, phase) {
		return nil
	}
	return r.Status().Update(ctx, addr)
}

// releaseAfterDelay returns a Released address to the pool once its
// class's releaseDelay has passed since it was Released, by clearing its
// claimRef; the next pass makes it Available. An address annotated held,
// or of a class without a delay, is left alone.
func (r *IPAddressReconciler) releaseAfterDelay(ctx context.Context, addr *localv1alpha1.IPAddress) (ctrl.Result, error) {
	if addr.Status.Phase != localv1alpha1.IPAddressReleased || addr.Annotations[localv1alpha1.HeldAnnotation] == localv1alpha1.AnnotationTrue {
		return ctrl.Result{}, nil
	}
	class := &localv1alpha1.IPAddressClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: addr.Spec.ClassName}, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if class.Spec.ReleaseDelay == nil || class.Spec.ReleaseDelay.Duration <= 0 {
		return ctrl.Result{}, nil
	}
	released := meta.FindStatusCondition(addr.Status.Conditions, localv1alpha1.ConditionBound)
	if released == nil || released.Reason != localv1alpha1.ReasonReleased {
		return ctrl.Result{}, nil
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	if remaining := released.LastTransitionTime.Add(class.Spec.ReleaseDelay.Duration).Sub(now()); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	ref := addr.Spec.ClaimRef
	addr.Spec.ClaimRef = nil
	if err := r.Update(ctx, addr); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(addr, "Normal", "ReleaseDelayElapsed",
		"released by claim %s/%s for %s; returned to the pool", ref.Namespace, ref.Name, class.Spec.ReleaseDelay.Duration)
	return ctrl.Result{}, nil
}

// markPhase sets a core-owned phase on the in-memory address together with
// its Bound condition, whose lastTransitionTime then dates the change. It
// reports whether anything changed.
func markPhase(addr *localv1alpha1.IPAddress, phase localv1alpha1.IPAddressPhase) bool {
	cond := metav1.Condition{
		Type:               localv1alpha1.ConditionBound,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: addr.Generation,
	}
	switch phase {
	case localv1alpha1.IPAddressBound:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, localv1alpha1.ReasonBound, "bound to its claim"
	case localv1alpha1.IPAddressReleased:
		cond.Reason, cond.Message = localv1alpha1.ReasonReleased, "its claim was deleted under reclaimPolicy Retain"
	case localv1alpha1.IPAddressAvailable:
		cond.Reason, cond.Message = localv1alpha1.ReasonAvailable, "bound to no claim"
	default:
		cond.Reason, cond.Message = localv1alpha1.ReasonPending, "spec.address does not parse"
	}
	changed := addr.Status.Phase != phase
	addr.Status.Phase = phase
	return meta.SetStatusCondition(&addr.Status.Conditions, cond) || changed
}

// releasedAddressesForClass maps a class event to its Released addresses,
// so a changed releaseDelay takes effect.
func (r *IPAddressReconciler) releasedAddressesForClass(ctx context.Context, o client.Object) []reconcile.Request {
	list := &localv1alpha1.IPAddressList{}
	if err := r.List(ctx, list, client.MatchingFields{IPAddressClassNameIndex: o.GetName()}); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, addr := range list.Items {
		if addr.Status.Phase == localv1alpha1.IPAddressReleased {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: addr.Name}})
		}
	}
	return reqs
}

// addressesForClaim maps a claim event to the addresses bound to it, so
// claim deletion and rebinding propagate promptly.
func (r *IPAddressReconciler) addressesForClaim(ctx context.Context, o client.Object) []reconcile.Request {
	list := &localv1alpha1.IPAddressList{}
	if err := r.List(ctx, list, client.MatchingFields{
		IPAddressClaimRefIndex: ClaimRefIndexKey(o.GetNamespace(), o.GetName()),
	}); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, addr := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: addr.Name}})
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPAddressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&localv1alpha1.IPAddress{}).
		Watches(&localv1alpha1.IPAddressClaim{}, handler.EnqueueRequestsFromMapFunc(r.addressesForClaim)).
		Watches(&localv1alpha1.IPAddressClass{}, handler.EnqueueRequestsFromMapFunc(r.releasedAddressesForClass)).
		Named("ipaddress").
		Complete(r)
}
