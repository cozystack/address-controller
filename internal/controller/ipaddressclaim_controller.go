/*
Copyright 2026 Timofei Larkin.

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
	"fmt"
	"sort"

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

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
)

// IPAddressClaimReconciler owns the class-agnostic claim lifecycle: resolving
// the claim's class, stamping the provisioner annotation for the per-class
// driver, matching or accepting IPAddress bindings, and running the reclaim
// flow when the claim is deleted. It never allocates addresses itself.
type IPAddressClaimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclaims,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives an IPAddressClaim towards Bound.
func (r *IPAddressClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	claim := &localv1alpha1.IPAddressClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalizeClaim(ctx, claim)
	}

	if !controllerutil.ContainsFinalizer(claim, localv1alpha1.ClaimProtectionFinalizer) {
		controllerutil.AddFinalizer(claim, localv1alpha1.ClaimProtectionFinalizer)
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Binding state comes first, and the class is consulted only when
	// something is still missing: a fully bound claim must keep working —
	// and keep its status maintained — even if its class was deleted.
	bound, err := r.addressesBoundTo(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.completePreBindings(ctx, claim, bound); err != nil {
		return ctrl.Result{}, err
	}

	className := ""
	if len(missingFamilies(claim, bound)) > 0 {
		className, err = r.resolveClass(ctx, claim)
		if err != nil {
			return ctrl.Result{}, err
		}
		if className != "" {
			for _, family := range missingFamilies(claim, bound) {
				matched, err := r.bindAvailableAddress(ctx, claim, className, family)
				if err != nil {
					return ctrl.Result{}, err
				}
				if matched != nil {
					bound = append(bound, *matched)
				}
			}
		}
	}

	if err := r.updateClaimStatus(ctx, claim, className, bound); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciled claim", "phase", claim.Status.Phase)
	return ctrl.Result{}, nil
}

// finalizeClaim runs the reclaim flow for every address bound to a deleted
// claim, then drops the protection finalizer.
func (r *IPAddressClaimReconciler) finalizeClaim(ctx context.Context, claim *localv1alpha1.IPAddressClaim) error {
	if !controllerutil.ContainsFinalizer(claim, localv1alpha1.ClaimProtectionFinalizer) {
		return nil
	}
	addrs, err := r.addressesBoundTo(ctx, claim)
	if err != nil {
		return err
	}
	for i := range addrs {
		addr := &addrs[i]
		if !addr.DeletionTimestamp.IsZero() {
			continue
		}
		switch addr.Spec.ReclaimPolicy {
		case localv1alpha1.ReclaimDelete:
			// The driver's own finalizer tears down the backend allocation
			// before the object goes away.
			if err := r.Delete(ctx, addr); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			r.Recorder.Eventf(claim, "Normal", "AddressDeleted",
				"IPAddress %s deleted per reclaimPolicy Delete", addr.Name)
		default: // Retain
			if addr.Status.Phase != localv1alpha1.IPAddressReleased {
				addr.Status.Phase = localv1alpha1.IPAddressReleased
				if err := r.Status().Update(ctx, addr); err != nil {
					return err
				}
			}
			r.Recorder.Eventf(claim, "Normal", "AddressReleased",
				"IPAddress %s released per reclaimPolicy Retain", addr.Name)
		}
	}
	controllerutil.RemoveFinalizer(claim, localv1alpha1.ClaimProtectionFinalizer)
	return r.Update(ctx, claim)
}

// resolveClass resolves the claim's class name — spec first, then the sticky
// status record, then the default class — verifies the class exists, and
// stamps the provisioner annotation. An empty return with nil error means the
// claim cannot resolve yet; the reason is already recorded on the in-memory
// conditions, persisted by the caller's status update.
func (r *IPAddressClaimReconciler) resolveClass(ctx context.Context, claim *localv1alpha1.IPAddressClaim) (string, error) {
	className := claim.Spec.ClassName
	if className == "" {
		className = claim.Status.ClassName
	}
	if className == "" {
		var err error
		className, err = r.defaultClassName(ctx, claim)
		if err != nil || className == "" {
			return "", err
		}
	}

	class := &localv1alpha1.IPAddressClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: className}, class); err != nil {
		if apierrors.IsNotFound(err) {
			r.markUnresolved(claim, localv1alpha1.ReasonClassNotFound,
				fmt.Sprintf("IPAddressClass %q not found", className))
			return "", nil
		}
		return "", err
	}

	// The stamp always mirrors the resolved class's provisioner; if the
	// claim is re-targeted at a different class before binding, drivers
	// must see the new name.
	if claim.Annotations[localv1alpha1.ProvisionerAnnotation] != class.Spec.Provisioner {
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[localv1alpha1.ProvisionerAnnotation] = class.Spec.Provisioner
		if err := r.Update(ctx, claim); err != nil {
			return "", err
		}
	}
	return className, nil
}

// defaultClassName finds the single IPAddressClass annotated as default. Zero
// or more than one default is a recorded, non-retriable condition.
func (r *IPAddressClaimReconciler) defaultClassName(ctx context.Context, claim *localv1alpha1.IPAddressClaim) (string, error) {
	classes := &localv1alpha1.IPAddressClassList{}
	if err := r.List(ctx, classes); err != nil {
		return "", err
	}
	var defaults []string
	for _, c := range classes.Items {
		if c.Annotations[localv1alpha1.IsDefaultClassAnnotation] == "true" {
			defaults = append(defaults, c.Name)
		}
	}
	switch len(defaults) {
	case 1:
		return defaults[0], nil
	case 0:
		r.markUnresolved(claim, localv1alpha1.ReasonNoDefaultClass,
			"claim names no class and no IPAddressClass is annotated as default")
		return "", nil
	default:
		sort.Strings(defaults)
		r.markUnresolved(claim, localv1alpha1.ReasonMultipleDefaultClasses,
			fmt.Sprintf("multiple IPAddressClasses annotated as default: %v", defaults))
		return "", nil
	}
}

// markUnresolved records a failed class resolution on the in-memory object;
// the caller's status update persists it.
func (r *IPAddressClaimReconciler) markUnresolved(claim *localv1alpha1.IPAddressClaim, reason, message string) {
	r.Recorder.Event(claim, "Warning", reason, message)
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               localv1alpha1.ConditionClassResolved,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: claim.Generation,
	})
}

// completePreBindings accepts driver pre-bound addresses by writing the
// claim's UID into claimRefs that carry none yet.
func (r *IPAddressClaimReconciler) completePreBindings(ctx context.Context, claim *localv1alpha1.IPAddressClaim, addrs []localv1alpha1.IPAddress) error {
	for i := range addrs {
		addr := &addrs[i]
		if addr.Spec.ClaimRef.UID == "" {
			addr.Spec.ClaimRef.UID = claim.UID
			if err := r.Update(ctx, addr); err != nil {
				return err
			}
		}
	}
	return nil
}

// addressesBoundTo lists live addresses whose claimRef names this claim and
// whose UID is unset or matches. A UID mismatch is a stale binding to an
// earlier claim of the same name and is not ours.
func (r *IPAddressClaimReconciler) addressesBoundTo(ctx context.Context, claim *localv1alpha1.IPAddressClaim) ([]localv1alpha1.IPAddress, error) {
	list := &localv1alpha1.IPAddressList{}
	if err := r.List(ctx, list, client.MatchingFields{
		IPAddressClaimRefIndex: ClaimRefIndexKey(claim.Namespace, claim.Name),
	}); err != nil {
		return nil, err
	}
	var addrs []localv1alpha1.IPAddress
	for _, addr := range list.Items {
		if !addr.DeletionTimestamp.IsZero() {
			continue
		}
		if addr.Spec.ClaimRef.UID != "" && addr.Spec.ClaimRef.UID != claim.UID {
			continue
		}
		addrs = append(addrs, addr)
	}
	return addrs, nil
}

// requestedFamilies expands the claim's family into the concrete families it
// needs bound.
func requestedFamilies(claim *localv1alpha1.IPAddressClaim) []localv1alpha1.AddressFamily {
	switch claim.Spec.Family {
	case localv1alpha1.FamilyIPv6:
		return []localv1alpha1.AddressFamily{localv1alpha1.FamilyIPv6}
	case localv1alpha1.FamilyDual:
		return []localv1alpha1.AddressFamily{localv1alpha1.FamilyIPv4, localv1alpha1.FamilyIPv6}
	default:
		return []localv1alpha1.AddressFamily{localv1alpha1.FamilyIPv4}
	}
}

// missingFamilies reports which requested families no bound address
// satisfies. An address in phase Lost does not satisfy its family.
func missingFamilies(claim *localv1alpha1.IPAddressClaim, addrs []localv1alpha1.IPAddress) []localv1alpha1.AddressFamily {
	var missing []localv1alpha1.AddressFamily
	for _, family := range requestedFamilies(claim) {
		satisfied := false
		for _, addr := range addrs {
			if addr.Status.Phase != localv1alpha1.IPAddressLost && familyOf(addr.Spec.Address) == family {
				satisfied = true
				break
			}
		}
		if !satisfied {
			missing = append(missing, family)
		}
	}
	return missing
}

// bindAvailableAddress binds one Available address of the wanted class and
// family to the claim, honouring spec.addressName when set. Returning
// (nil, nil) means nothing matched and the claim keeps waiting for its
// driver to provision.
func (r *IPAddressClaimReconciler) bindAvailableAddress(ctx context.Context, claim *localv1alpha1.IPAddressClaim, className string, family localv1alpha1.AddressFamily) (*localv1alpha1.IPAddress, error) {
	var candidates []localv1alpha1.IPAddress
	if claim.Spec.AddressName != "" {
		addr := &localv1alpha1.IPAddress{}
		err := r.Get(ctx, types.NamespacedName{Name: claim.Spec.AddressName}, addr)
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		candidates = []localv1alpha1.IPAddress{*addr}
	} else {
		list := &localv1alpha1.IPAddressList{}
		if err := r.List(ctx, list); err != nil {
			return nil, err
		}
		candidates = list.Items
	}

	var match *localv1alpha1.IPAddress
	for i := range candidates {
		addr := &candidates[i]
		if addr.Spec.ClaimRef != nil || !addr.DeletionTimestamp.IsZero() {
			continue
		}
		if addr.Status.Phase != localv1alpha1.IPAddressAvailable {
			continue
		}
		if addr.Spec.ClassName != className || familyOf(addr.Spec.Address) != family {
			continue
		}
		if match == nil || addr.Name < match.Name {
			match = addr
		}
	}
	if match == nil {
		return nil, nil
	}
	match.Spec.ClaimRef = &localv1alpha1.ClaimReference{
		Namespace: claim.Namespace,
		Name:      claim.Name,
		UID:       claim.UID,
	}
	// A conflict here means someone bound it first; the returned error
	// requeues the claim and the next pass picks another candidate.
	if err := r.Update(ctx, match); err != nil {
		return nil, err
	}
	r.Recorder.Eventf(claim, "Normal", "Matched", "bound Available IPAddress %s", match.Name)
	return match, nil
}

// updateClaimStatus recomputes phase, bound-address list, and conditions.
// An empty className means resolution was not needed (nothing missing) or
// not possible this pass (conditions already say why); the sticky
// status.className is left untouched then.
func (r *IPAddressClaimReconciler) updateClaimStatus(ctx context.Context, claim *localv1alpha1.IPAddressClaim, className string, bound []localv1alpha1.IPAddress) error {
	previous := claim.Status.Phase

	sort.Slice(bound, func(i, j int) bool { return bound[i].Name < bound[j].Name })
	var reported []localv1alpha1.BoundAddress
	for _, addr := range bound {
		reported = append(reported, localv1alpha1.BoundAddress{Name: addr.Name, Address: addr.Spec.Address})
	}

	claim.Status.Addresses = reported
	if className != "" {
		claim.Status.ClassName = className
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               localv1alpha1.ConditionClassResolved,
			Status:             metav1.ConditionTrue,
			Reason:             localv1alpha1.ReasonResolved,
			Message:            fmt.Sprintf("resolved to IPAddressClass %q", className),
			ObservedGeneration: claim.Generation,
		})
	}

	missing := missingFamilies(claim, bound)
	switch {
	case len(missing) == 0:
		claim.Status.Phase = localv1alpha1.ClaimBound
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               localv1alpha1.ConditionBound,
			Status:             metav1.ConditionTrue,
			Reason:             localv1alpha1.ReasonBound,
			Message:            "all requested families are bound",
			ObservedGeneration: claim.Generation,
		})
	case previous == localv1alpha1.ClaimBound || previous == localv1alpha1.ClaimLost:
		claim.Status.Phase = localv1alpha1.ClaimLost
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               localv1alpha1.ConditionBound,
			Status:             metav1.ConditionFalse,
			Reason:             localv1alpha1.ReasonAddressLost,
			Message:            fmt.Sprintf("bound address for families %v disappeared", missing),
			ObservedGeneration: claim.Generation,
		})
	default:
		claim.Status.Phase = localv1alpha1.ClaimPending
		message := fmt.Sprintf("waiting for class resolution before families %v can be provisioned", missing)
		if provisioner := claim.Annotations[localv1alpha1.ProvisionerAnnotation]; provisioner != "" {
			message = fmt.Sprintf("waiting for provisioner %q to provision families %v", provisioner, missing)
		}
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               localv1alpha1.ConditionBound,
			Status:             metav1.ConditionFalse,
			Reason:             localv1alpha1.ReasonWaitingForProvisioning,
			Message:            message,
			ObservedGeneration: claim.Generation,
		})
	}

	if err := r.Status().Update(ctx, claim); err != nil {
		return err
	}
	if claim.Status.Phase != previous {
		r.Recorder.Eventf(claim, "Normal", string(claim.Status.Phase),
			"claim is %s", claim.Status.Phase)
	}
	return nil
}

// claimsForAddress maps an IPAddress event to the claim it is bound to, or —
// for unbound addresses — to every claim still awaiting binding.
func (r *IPAddressClaimReconciler) claimsForAddress(ctx context.Context, o client.Object) []reconcile.Request {
	addr := o.(*localv1alpha1.IPAddress)
	if ref := addr.Spec.ClaimRef; ref != nil {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}}
	}
	return r.unboundClaims(ctx)
}

// claimsForClass maps an IPAddressClass event to every claim still awaiting
// binding — a new or changed class may unblock resolution.
func (r *IPAddressClaimReconciler) claimsForClass(ctx context.Context, _ client.Object) []reconcile.Request {
	return r.unboundClaims(ctx)
}

func (r *IPAddressClaimReconciler) unboundClaims(ctx context.Context) []reconcile.Request {
	claims := &localv1alpha1.IPAddressClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, c := range claims.Items {
		if c.Status.Phase != localv1alpha1.ClaimBound {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: c.Namespace, Name: c.Name}})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPAddressClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&localv1alpha1.IPAddressClaim{}).
		Watches(&localv1alpha1.IPAddress{}, handler.EnqueueRequestsFromMapFunc(r.claimsForAddress)).
		Watches(&localv1alpha1.IPAddressClass{}, handler.EnqueueRequestsFromMapFunc(r.claimsForClass)).
		Named("ipaddressclaim").
		Complete(r)
}
