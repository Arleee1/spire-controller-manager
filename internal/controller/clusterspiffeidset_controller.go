package controller

import (
	"context"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/pkg/reconciler"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ClusterSPIFFEIDSetReconciler triggers the shared entry pass without fixed input watches.
type ClusterSPIFFEIDSetReconciler struct {
	Triggerer reconciler.Triggerer
}

// +kubebuilder:rbac:groups=spire.spiffe.io,resources=clusterspiffeidsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=spire.spiffe.io,resources=clusterspiffeidsets/status,verbs=get;update;patch

func (r *ClusterSPIFFEIDSetReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log.FromContext(ctx).V(1).Info("Triggering explicit-input identity reconciliation")
	r.Triggerer.Trigger()
	return ctrl.Result{}, nil
}

func (r *ClusterSPIFFEIDSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spirev1alpha1.ClusterSPIFFEIDSet{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
