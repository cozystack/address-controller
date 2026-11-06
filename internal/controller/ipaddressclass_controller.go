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
	"sort"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

// IPAddressClassReconciler guards class deletion: a deleted class is held
// by the class-protection finalizer until no IPAddress of the class is left.
type IPAddressClassReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile adds the protection finalizer, or releases a deleted class once
// its addresses are gone.
func (r *IPAddressClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	class := &localv1alpha1.IPAddressClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if class.DeletionTimestamp.IsZero() {
		if controllerutil.AddFinalizer(class, localv1alpha1.ClassProtectionFinalizer) {
			return ctrl.Result{}, r.Update(ctx, class)
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(class, localv1alpha1.ClassProtectionFinalizer) {
		return ctrl.Result{}, nil
	}

	addrs := &localv1alpha1.IPAddressList{}
	if err := r.List(ctx, addrs, client.MatchingFields{IPAddressClassNameIndex: class.Name}); err != nil {
		return ctrl.Result{}, err
	}
	if len(addrs.Items) > 0 {
		names := make([]string, 0, len(addrs.Items))
		for _, addr := range addrs.Items {
			names = append(names, addr.Name)
		}
		sort.Strings(names)
		r.Recorder.Eventf(class, "Warning", localv1alpha1.ReasonClassInUse,
			"deletion waits for %d IPAddress(es) of this class: %v", len(names), names)
		return ctrl.Result{}, nil
	}
	controllerutil.RemoveFinalizer(class, localv1alpha1.ClassProtectionFinalizer)
	return ctrl.Result{}, r.Update(ctx, class)
}

// classForAddress maps an IPAddress event to the address's class.
func (r *IPAddressClassReconciler) classForAddress(_ context.Context, o client.Object) []reconcile.Request {
	addr := o.(*localv1alpha1.IPAddress)
	if addr.Spec.ClassName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: addr.Spec.ClassName}}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPAddressClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&localv1alpha1.IPAddressClass{}).
		Watches(&localv1alpha1.IPAddress{}, handler.EnqueueRequestsFromMapFunc(r.classForAddress)).
		Named("ipaddressclass").
		Complete(r)
}
