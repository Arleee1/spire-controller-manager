package controller

import (
	"context"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/pkg/reconciler"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ClusterServiceAccountSPIFFEIDReconciler triggers the shared entry reconciler.
type ClusterServiceAccountSPIFFEIDReconciler struct {
	Triggerer reconciler.Triggerer
}

// +kubebuilder:rbac:groups=spire.spiffe.io,resources=clusterserviceaccountspiffeids,verbs=get;list;watch
// +kubebuilder:rbac:groups=spire.spiffe.io,resources=clusterserviceaccountspiffeids/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces;serviceaccounts,verbs=get;list;watch

func (r *ClusterServiceAccountSPIFFEIDReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log.FromContext(ctx).V(1).Info("Triggering namespace/service-account reconciliation")
	r.Triggerer.Trigger()
	return ctrl.Result{}, nil
}

// SetupWithManager watches only the rule, namespaces and service accounts.
func (r *ClusterServiceAccountSPIFFEIDReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spirev1alpha1.ClusterServiceAccountSPIFFEID{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Namespace{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.ServiceAccount{}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
