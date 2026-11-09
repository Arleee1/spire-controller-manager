package spireentry

import (
	"context"
	"fmt"
	"slices"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/pkg/k8sinput"
	"github.com/spiffe/spire-controller-manager/pkg/namespace"
	"github.com/spiffe/spire-controller-manager/pkg/spireapi"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type inputTemplateData struct {
	TrustDomain string
	ClusterName string
	Inputs      map[string]map[string]any
}

func (r *entryReconciler) listClusterSPIFFEIDSets(ctx context.Context) ([]*ClusterSPIFFEIDSet, error) {
	var list spirev1alpha1.ClusterSPIFFEIDSetList
	if err := r.config.K8sClient.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []*ClusterSPIFFEIDSet
	for _, obj := range list.Items {
		if !r.reconcileClass(obj.Spec.ClassName) || obj.DeletionTimestamp != nil {
			continue
		}
		by := &ClusterSPIFFEIDSet{
			ClusterSPIFFEIDSet: obj,
			NextStatus: spirev1alpha1.ClusterSPIFFEIDSetStatus{
				ObservedGeneration: obj.Generation, Conditions: slices.Clone(obj.Status.Conditions),
			},
		}
		by.setCondition(metav1.ConditionTrue, "Reconciled", "Desired explicit-input entries reconciled with SPIRE")
		out = append(out, by)
	}
	return out, nil
}

func (r *entryReconciler) addClusterSPIFFEIDSetEntriesState(ctx context.Context, state entriesState, objects []*ClusterSPIFFEIDSet) error {
	specs := make(map[*ClusterSPIFFEIDSet]*spirev1alpha1.ParsedClusterSPIFFEIDSetSpec)
	kinds := make(map[schema.GroupVersionKind]k8sinput.Kind)
	var declaredKinds []schema.GroupVersionKind
	for _, by := range objects {
		spec, err := spirev1alpha1.ParseClusterSPIFFEIDSetSpec(&by.Spec)
		if err != nil {
			log.FromContext(ctx).Error(err, "Invalid ClusterSPIFFEIDSet", "clusterSPIFFEIDSet", by.Name)
			by.setCondition(metav1.ConditionFalse, "InvalidSpec", err.Error())
			continue
		}
		specs[by] = spec
		for _, input := range spec.ForEach {
			declaredKinds = append(declaredKinds, input.GVK)
		}
	}
	if r.config.Inputs != nil {
		r.config.Inputs.Retain(declaredKinds)
	}
	for _, by := range objects {
		spec := specs[by]
		if spec == nil {
			continue
		}
		valid := true
		var ruleKinds []k8sinput.Kind
		for _, input := range spec.ForEach {
			if r.config.Inputs == nil {
				return fmt.Errorf("input cache is not configured for %s", by.Name)
			}
			kind, err := r.config.Inputs.Resolve(input.GVK)
			if err != nil {
				return fmt.Errorf("%s input %s: %w", by.Name, input.Alias, err)
			}
			if input.NamespaceTemplate != nil && !kind.Namespaced {
				err := fmt.Errorf("input %s is cluster-scoped and cannot use namespaceTemplate", input.Alias)
				log.FromContext(ctx).Error(err, "Invalid ClusterSPIFFEIDSet", "clusterSPIFFEIDSet", by.Name)
				by.setCondition(metav1.ConditionFalse, "InvalidSpec", err.Error())
				valid = false
				break
			}
			ruleKinds = append(ruleKinds, kind)
		}
		if valid {
			for _, kind := range ruleKinds {
				kinds[kind.GVK] = kind
			}
		} else {
			delete(specs, by)
		}
	}
	if r.config.Inputs != nil {
		wanted := make([]k8sinput.Kind, 0, len(kinds))
		for _, kind := range kinds {
			wanted = append(wanted, kind)
		}
		if err := r.config.Inputs.Sync(ctx, wanted); err != nil {
			return err
		}
		// Check every declared kind, including downstream joins with no current matches.
		for gvk := range kinds {
			if err := r.config.Inputs.Check(gvk); err != nil {
				return err
			}
		}
	}
	for _, by := range objects {
		spec := specs[by]
		if spec == nil {
			continue
		}
		data := &inputTemplateData{TrustDomain: r.config.TrustDomain.Name(), ClusterName: r.config.ClusterName, Inputs: make(map[string]map[string]any)}
		declared := make(map[entryKey]struct{})
		if err := r.walkInputs(ctx, state, by, spec, data, declared, 0); err != nil {
			return err
		}
	}
	for gvk := range kinds {
		if err := r.config.Inputs.Check(gvk); err != nil {
			return err
		}
	}
	return nil
}

func (r *entryReconciler) walkInputs(ctx context.Context, state entriesState, by *ClusterSPIFFEIDSet, spec *spirev1alpha1.ParsedClusterSPIFFEIDSetSpec, data *inputTemplateData, declared map[entryKey]struct{}, index int) error {
	renderFailure := func(err error) {
		log.FromContext(ctx).Error(err, "Failed to render identity-set entry", "clusterSPIFFEIDSet", by.Name)
		by.NextStatus.Stats.EntryRenderFailures++
		by.setCondition(metav1.ConditionFalse, "RenderFailed", err.Error())
	}
	if index == len(spec.ForEach) {
		by.NextStatus.Stats.Combinations++
		id, err := renderSPIFFEID(spec.SPIFFEIDTemplate, data, r.config.TrustDomain)
		if err != nil {
			renderFailure(fmt.Errorf("spiffeIDTemplate: %w", err))
			return nil
		}
		parent, err := renderSPIFFEID(spec.ParentIDTemplate, data, r.config.TrustDomain)
		if err != nil {
			renderFailure(fmt.Errorf("parentIDTemplate: %w", err))
			return nil
		}
		selectors, err := renderWorkloadSelectors(spec.WorkloadSelectorTemplates, data)
		if err != nil {
			renderFailure(err)
			return nil
		}
		entry := spireapi.Entry{SPIFFEID: id, ParentID: parent, Selectors: selectors}
		key := makeEntryKey(entry)
		if _, exists := declared[key]; !exists {
			declared[key] = struct{}{}
			// Share the established scoped IDs with the SA source for gap-free handoffs.
			entry.ID = r.serviceAccountEntryID(entry)
			state.AddDeclared(entry, by)
		}
		return nil
	}
	input := spec.ForEach[index]
	var inputNamespace string
	if input.NamespaceTemplate != nil {
		var err error
		inputNamespace, err = renderTemplate(input.NamespaceTemplate, data)
		if err != nil {
			renderFailure(fmt.Errorf("input %s namespaceTemplate: %w", input.Alias, err))
			return nil
		}
		if problems := validation.IsDNS1123Label(inputNamespace); len(problems) != 0 {
			renderFailure(fmt.Errorf("input %s namespaceTemplate must render a valid nonempty namespace: %v", input.Alias, problems))
			return nil
		}
	}
	objects, err := r.config.Inputs.List(input.GVK, inputNamespace, input.LabelSelector)
	if err != nil {
		return fmt.Errorf("%s input %s: %w", by.Name, input.Alias, err)
	}
	for _, object := range objects {
		objectNamespace := object.GetNamespace()
		if input.GVK.Group == "" && input.GVK.Kind == "Namespace" {
			objectNamespace = object.GetName()
		}
		if object.GetDeletionTimestamp() != nil || (objectNamespace != "" && namespace.IsIgnored(r.config.IgnoreNamespaces, objectNamespace)) {
			continue
		}
		data.Inputs[input.Alias] = object.Object
		if err := r.walkInputs(ctx, state, by, spec, data, declared, index+1); err != nil {
			return err
		}
		delete(data.Inputs, input.Alias)
	}
	return nil
}

func (r *entryReconciler) failIdentitySets(ctx context.Context, objects []*ClusterSPIFFEIDSet, reason string, err error) {
	for _, by := range objects {
		by.setCondition(metav1.ConditionFalse, reason, err.Error())
	}
	r.updateIdentitySetStatuses(ctx, objects)
}

func (r *entryReconciler) updateIdentitySetStatuses(ctx context.Context, objects []*ClusterSPIFFEIDSet) {
	for _, by := range objects {
		if equality.Semantic.DeepEqual(by.Status, by.NextStatus) {
			continue
		}
		by.Status = by.NextStatus
		if err := r.config.K8sClient.Status().Update(ctx, &by.ClusterSPIFFEIDSet); err != nil {
			log.FromContext(ctx).Error(err, "Failed to update identity-set status", "clusterSPIFFEIDSet", by.Name)
		}
	}
}
