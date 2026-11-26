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
	"slices"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// ServiceReconciler auto-claims addresses for LoadBalancer Services that
// name no claim: it creates an IPAddressClaim in the Service's namespace,
// controlled by the Service, and points the Service at it with the claim
// annotation. From there the class's driver associates the address exactly
// as for a tenant-made claim. The claim is named after the Service.
type ServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=services/finalizers,verbs=update
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclaims,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile auto-claims an address for one Service, if it qualifies.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !wantsAutoClaim(svc) {
		return ctrl.Result{}, nil
	}

	class, err := r.serviceClass(ctx, svc)
	if err != nil || class == nil {
		return ctrl.Result{}, err
	}
	if !sameLoadBalancerClass(svc.Spec.LoadBalancerClass, class.Spec.LoadBalancerClass) {
		// The Service reaches another load-balancer implementation than
		// the one owning the class's pool. Left alone when the class is the
		// marked one; reported when the Service named it.
		if svc.Annotations[localv1alpha1.ServiceClassAnnotation] != "" {
			r.Recorder.Eventf(svc, "Warning", localv1alpha1.ReasonLoadBalancerClassMismatch,
				"Service loadBalancerClass %s does not match IPAddressClass %q loadBalancerClass %s",
				describeLoadBalancerClass(svc.Spec.LoadBalancerClass), class.Name,
				describeLoadBalancerClass(class.Spec.LoadBalancerClass))
		}
		return ctrl.Result{}, nil
	}

	claim := &localv1alpha1.IPAddressClaim{}
	err = r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, claim)
	switch {
	case apierrors.IsNotFound(err):
		claim = &localv1alpha1.IPAddressClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: svc.Namespace, Name: svc.Name},
			Spec: localv1alpha1.IPAddressClaimSpec{
				ClassName: class.Name,
				Family:    familyForService(svc),
			},
		}
		if err := controllerutil.SetControllerReference(svc, claim, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	case err != nil:
		return ctrl.Result{}, err
	case !metav1.IsControlledBy(claim, svc):
		// A claim of that name already exists and is not ours — the
		// tenant's, or one retained from an earlier Service of the same
		// name. Adopting it is the tenant's call, via the claim annotation.
		r.Recorder.Eventf(svc, "Warning", localv1alpha1.ReasonAutoClaimNameTaken,
			"IPAddressClaim %q exists and is not controlled by this Service", svc.Name)
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(svc.DeepCopy())
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[localv1alpha1.ServiceClaimAnnotation] = claim.Name
	if err := r.Patch(ctx, svc, patch); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(svc, "Normal", localv1alpha1.ReasonAutoClaimed,
		"auto-claimed IPAddressClaim %q from IPAddressClass %q", claim.Name, claim.Spec.ClassName)
	logger.V(1).Info("auto-claimed", "claim", claim.Name, "class", claim.Spec.ClassName)
	return ctrl.Result{}, nil
}

// backendAddressInputs are the Service annotations through which a backend
// is told which pool to draw from, which address to use, or whom to share
// one with: the gated set of the design proposal's Security section, which
// also gates spec.loadBalancerIP. A Service carrying one has chosen its
// address source. The driver's own placeholders name their pool this way.
var backendAddressInputs = []string{
	"metallb.io/loadBalancerIPs",
	"metallb.io/address-pool",
	"metallb.io/allow-shared-ip",
	"metallb.universe.tf/loadBalancerIPs",
	"metallb.universe.tf/address-pool",
	"metallb.universe.tf/allow-shared-ip",
}

// wantsAutoClaim reports whether a Service qualifies for auto-claim: a live
// LoadBalancer Service that names no claim, has not opted out, carries no
// backend pool or pin input, and has no address yet. Whether its
// loadBalancerClass matches the class's is checked once the class is known. The last rule keeps a Service addressed by
// other means — before auto-claim existed, or from an auto-assigning pool —
// from being moved to a new address.
func wantsAutoClaim(svc *corev1.Service) bool {
	return svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		svc.DeletionTimestamp.IsZero() &&
		svc.Annotations[localv1alpha1.ServiceClaimAnnotation] == "" &&
		svc.Annotations[localv1alpha1.ServiceAutoClaimAnnotation] != "false" &&
		svc.Spec.LoadBalancerIP == "" &&
		!slices.ContainsFunc(backendAddressInputs, func(key string) bool {
			_, ok := svc.Annotations[key]
			return ok
		}) &&
		len(svc.Status.LoadBalancer.Ingress) == 0
}

// serviceClass returns the class a Service's auto-claim draws from: the one
// its class annotation names, else the single class marked for auto-claim.
// Nil means nothing is claimed for now. A named class that does not exist
// is recorded on the Service; creating it requeues the Service.
func (r *ServiceReconciler) serviceClass(ctx context.Context, svc *corev1.Service) (*localv1alpha1.IPAddressClass, error) {
	name := svc.Annotations[localv1alpha1.ServiceClassAnnotation]
	if name == "" {
		return r.autoClaimClass(ctx, svc)
	}
	class := &localv1alpha1.IPAddressClass{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, class)
	if apierrors.IsNotFound(err) {
		r.Recorder.Eventf(svc, "Warning", localv1alpha1.ReasonClassNotFound,
			"IPAddressClass %q not found", name)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return class, nil
}

// sameLoadBalancerClass reports whether two loadBalancerClass values are
// equal, both unset counting as equal.
func sameLoadBalancerClass(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func describeLoadBalancerClass(v *string) string {
	if v == nil {
		return "(unset)"
	}
	return strconv.Quote(*v)
}

// autoClaimClass finds the single IPAddressClass marked for auto-claim.
// None means auto-claim is off and the Service is left alone; more than one
// is recorded on the Service and nothing is claimed.
func (r *ServiceReconciler) autoClaimClass(ctx context.Context, svc *corev1.Service) (*localv1alpha1.IPAddressClass, error) {
	classes := &localv1alpha1.IPAddressClassList{}
	if err := r.List(ctx, classes); err != nil {
		return nil, err
	}
	// A deleted class is held only until its addresses are gone; drawing
	// new claims into it would keep it from draining.
	classes.Items = slices.DeleteFunc(classes.Items, classDeleting)
	var marked []localv1alpha1.IPAddressClass
	for _, c := range classes.Items {
		if c.Annotations[localv1alpha1.IsAutoClaimClassAnnotation] == localv1alpha1.AnnotationTrue {
			marked = append(marked, c)
		}
	}
	switch len(marked) {
	case 0:
		return nil, nil
	case 1:
		return &marked[0], nil
	default:
		names := make([]string, 0, len(marked))
		for _, c := range marked {
			names = append(names, c.Name)
		}
		sort.Strings(names)
		r.Recorder.Eventf(svc, "Warning", localv1alpha1.ReasonMultipleAutoClaimClasses,
			"multiple IPAddressClasses marked for auto-claim: %v", names)
		return nil, nil
	}
}

// familyForService derives the claim family from the Service's IP families.
// Only RequireDualStack asks for both: a PreferDualStack Service would
// rather have one address than wait on a class that cannot supply two.
func familyForService(svc *corev1.Service) localv1alpha1.AddressFamily {
	families := svc.Spec.IPFamilies
	if len(families) == 2 && svc.Spec.IPFamilyPolicy != nil &&
		*svc.Spec.IPFamilyPolicy == corev1.IPFamilyPolicyRequireDualStack {
		return localv1alpha1.FamilyDual
	}
	if len(families) > 0 && families[0] == corev1.IPv6Protocol {
		return localv1alpha1.FamilyIPv6
	}
	return localv1alpha1.FamilyIPv4
}

// servicesForClass maps an IPAddressClass event to every Service that may
// now qualify — a class newly marked for auto-claim, or one named by a
// Service annotation, may unblock it.
func (r *ServiceReconciler) servicesForClass(ctx context.Context, _ client.Object) []reconcile.Request {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, s := range services.Items {
		if wantsAutoClaim(&s) {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: s.Namespace, Name: s.Name}})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&localv1alpha1.IPAddressClass{}, handler.EnqueueRequestsFromMapFunc(r.servicesForClass)).
		Named("service").
		Complete(r)
}
