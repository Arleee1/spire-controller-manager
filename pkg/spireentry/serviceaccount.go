package spireentry

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/pkg/k8sapi"
	"github.com/spiffe/spire-controller-manager/pkg/namespace"
	"github.com/spiffe/spire-controller-manager/pkg/spireapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const serviceAccountEntryIDMarker = "serviceaccount."

type serviceAccountTemplateData struct {
	TrustDomain        string
	ClusterName        string
	NamespaceMeta      *metav1.ObjectMeta
	ServiceAccountMeta *metav1.ObjectMeta
}

func renderServiceAccountEntry(spec *spirev1alpha1.ParsedClusterServiceAccountSPIFFEIDSpec, ns *corev1.Namespace, sa *corev1.ServiceAccount, trustDomain spiffeid.TrustDomain, clusterName string) (*spireapi.Entry, error) {
	data := &serviceAccountTemplateData{
		TrustDomain:        trustDomain.Name(),
		ClusterName:        clusterName,
		NamespaceMeta:      &ns.ObjectMeta,
		ServiceAccountMeta: &sa.ObjectMeta,
	}
	id, err := renderSPIFFEID(spec.SPIFFEIDTemplate, data, trustDomain)
	if err != nil {
		return nil, fmt.Errorf("failed to render SPIFFE ID: %w", err)
	}
	parentID, err := renderSPIFFEID(spec.ParentIDTemplate, data, trustDomain)
	if err != nil {
		return nil, fmt.Errorf("failed to render parent ID: %w", err)
	}
	return &spireapi.Entry{
		SPIFFEID: id,
		ParentID: parentID,
		Selectors: []spireapi.Selector{
			{Type: "k8s", Value: "ns:" + ns.Name},
			{Type: "k8s", Value: "sa:" + sa.Name},
		},
	}, nil
}

func (r *entryReconciler) serviceAccountEntryIDPrefix() string {
	scope := r.config.TrustDomain.Name() + "\x00" + r.config.ClusterName + "\x00" + r.config.ClassName
	return r.config.EntryIDPrefix + serviceAccountEntryIDMarker + uuid.NewSHA1(uuid.NameSpaceOID, []byte(scope)).String() + "."
}

func (r *entryReconciler) listClusterServiceAccountSPIFFEIDs(ctx context.Context) ([]*ClusterServiceAccountSPIFFEID, error) {
	objects, err := k8sapi.ListClusterServiceAccountSPIFFEIDs(ctx, r.config.K8sClient)
	if err != nil {
		return nil, err
	}
	out := make([]*ClusterServiceAccountSPIFFEID, 0, len(objects))
	for _, obj := range objects {
		if !r.reconcileClass(obj.Spec.ClassName) || obj.DeletionTimestamp != nil {
			continue
		}
		by := &ClusterServiceAccountSPIFFEID{
			ClusterServiceAccountSPIFFEID: obj,
			NextStatus: spirev1alpha1.ClusterServiceAccountSPIFFEIDStatus{
				ObservedGeneration: obj.Generation,
				Conditions:         slices.Clone(obj.Status.Conditions),
			},
		}
		by.setCondition(metav1.ConditionTrue, "Reconciled", "Desired service-account entries reconciled with SPIRE")
		out = append(out, by)
	}
	return out, nil
}

func (r *entryReconciler) addClusterServiceAccountSPIFFEIDEntriesState(ctx context.Context, state entriesState, objects []*ClusterServiceAccountSPIFFEID) error {
	for _, by := range objects {
		logger := log.FromContext(ctx).WithValues("clusterServiceAccountSPIFFEID", objectName(by))
		spec, err := spirev1alpha1.ParseClusterServiceAccountSPIFFEIDSpec(&by.Spec)
		if err != nil {
			logger.Error(err, "Failed to parse ClusterServiceAccountSPIFFEID spec")
			by.setCondition(metav1.ConditionFalse, "InvalidSpec", err.Error())
			continue
		}
		namespaces, err := r.listNamespaces(ctx, spec.NamespaceSelector)
		if err != nil {
			return fmt.Errorf("failed to list namespaces for %s: %w", by.Name, err)
		}
		by.NextStatus.Stats.NamespacesSelected = len(namespaces)
		for i := range namespaces {
			ns := &namespaces[i]
			if namespace.IsIgnored(r.config.IgnoreNamespaces, ns.Name) {
				by.NextStatus.Stats.NamespacesIgnored++
				continue
			}
			if ns.DeletionTimestamp != nil {
				continue
			}
			accounts, err := k8sapi.ListNamespaceServiceAccounts(ctx, r.config.K8sClient, ns.Name, spec.ServiceAccountSelector)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("failed to list service accounts in namespace %s: %w", ns.Name, err)
			}
			by.NextStatus.Stats.ServiceAccountsSelected += len(accounts)
			for j := range accounts {
				sa := &accounts[j]
				if sa.DeletionTimestamp != nil {
					continue
				}
				entry, err := renderServiceAccountEntry(spec, ns, sa, r.config.TrustDomain, r.config.ClusterName)
				if err != nil {
					logger.Error(err, "Failed to render service-account entry", "namespace", ns.Name, "serviceAccount", sa.Name)
					by.NextStatus.Stats.EntryRenderFailures++
					by.setCondition(metav1.ConditionFalse, "RenderFailed", fmt.Sprintf("%s/%s: %s", ns.Name, sa.Name, err))
					continue
				}
				// Stable, scoped IDs survive retries and restarts without adopting unrelated entries.
				entry.ID = r.serviceAccountEntryIDPrefix() + uuid.NewSHA1(uuid.NameSpaceOID, []byte(makeEntryKey(*entry))).String()
				state.AddDeclared(*entry, by)
			}
		}
	}
	return nil
}

func (r *entryReconciler) failServiceAccountReconciliation(ctx context.Context, objects []*ClusterServiceAccountSPIFFEID, reason string, err error) {
	for _, by := range objects {
		by.setCondition(metav1.ConditionFalse, reason, err.Error())
	}
	r.updateServiceAccountStatuses(ctx, objects)
}

func (r *entryReconciler) updateServiceAccountStatuses(ctx context.Context, objects []*ClusterServiceAccountSPIFFEID) {
	for _, by := range objects {
		if equality.Semantic.DeepEqual(by.Status, by.NextStatus) {
			continue
		}
		by.Status = by.NextStatus
		logger := log.FromContext(ctx).WithValues("clusterServiceAccountSPIFFEID", objectName(by))
		if err := r.config.K8sClient.Status().Update(ctx, &by.ClusterServiceAccountSPIFFEID); err != nil {
			logger.Error(err, "Failed to update status")
		} else {
			logger.Info("Updated status")
		}
	}
}
