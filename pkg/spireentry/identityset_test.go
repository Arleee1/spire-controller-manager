package spireentry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/pkg/k8sinput"
	"github.com/spiffe/spire-controller-manager/pkg/metrics"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIdentitySetIterationDomains(t *testing.T) {
	for _, tt := range []struct {
		name, identity string
		inputs         []spirev1alpha1.SPIFFEIDSetInput
		selectors      []string
		want           int
		kinds          []string
	}{
		{"constant", "spiffe://{{ .TrustDomain }}/constant", nil, []string{"custom:constant"}, 1, nil},
		{"namespace only", "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}", []spirev1alpha1.SPIFFEIDSetInput{setInput("ns", "Namespace", "")}, []string{"k8s:ns:{{ .Inputs.ns.metadata.name }}"}, 2, []string{"Namespace"}},
		{"service account only", "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.sa.metadata.namespace }}/sa/{{ .Inputs.sa.metadata.name }}", []spirev1alpha1.SPIFFEIDSetInput{setInput("sa", "ServiceAccount", "")}, []string{"k8s:sa:{{ .Inputs.sa.metadata.name }}"}, 3, []string{"ServiceAccount"}},
		{"namespace join", "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}/sa/{{ .Inputs.sa.metadata.name }}", []spirev1alpha1.SPIFFEIDSetInput{setInput("ns", "Namespace", ""), setInput("sa", "ServiceAccount", "{{ .Inputs.ns.metadata.name }}")}, []string{"k8s:ns:{{ .Inputs.ns.metadata.name }}", "k8s:sa:{{ .Inputs.sa.metadata.name }}"}, 3, []string{"Namespace", "ServiceAccount"}},
		{"independent inputs", "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}/sa-ns/{{ .Inputs.sa.metadata.namespace }}/sa/{{ .Inputs.sa.metadata.name }}", []spirev1alpha1.SPIFFEIDSetInput{setInput("ns", "Namespace", ""), setInput("sa", "ServiceAccount", "")}, []string{"custom:independent"}, 6, []string{"Namespace", "ServiceAccount"}},
		{"configmap only", "spiffe://{{ .TrustDomain }}/{{ .Inputs.config.data.identity }}", []spirev1alpha1.SPIFFEIDSetInput{setInput("config", "ConfigMap", "")}, []string{"custom:{{ .Inputs.config.data.selector }}"}, 1, []string{"ConfigMap"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rule := constantSet("set")
			rule.Spec.ForEach, rule.Spec.SPIFFEIDTemplate, rule.Spec.WorkloadSelectorTemplates = tt.inputs, tt.identity, tt.selectors
			r, k8s, store, inputs := newIdentitySetTest(t, rule,
				serviceAccountNamespace("alpha", true), serviceAccountNamespace("beta", true),
				serviceAccount("alpha", "one"), serviceAccount("alpha", "two"), serviceAccount("beta", "one"),
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "identity", Namespace: "alpha"}, Data: map[string]string{"identity": "configmap", "selector": "value:with:colons"}})
			r.reconcile(context.Background())
			require.Len(t, store.entries, tt.want)
			require.ElementsMatch(t, tt.kinds, inputs.activeKinds())
			require.Equal(t, tt.want, setStatus(t, k8s, rule.Name).Stats.Combinations)
			for _, entry := range store.entries {
				for _, selector := range entry.Selectors {
					require.NotContains(t, selector.Value, "pod-uid")
				}
			}
			if tt.name == "namespace join" {
				require.Equal(t, []string{"alpha", "beta"}, inputs.namespaces["ServiceAccount"], "joins must query only the per-namespace index")
			}
		})
	}
}

func TestIdentitySetCoalescingAndInputLifecycle(t *testing.T) {
	ctx := context.Background()
	rule := constantSet("set")
	rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("sa", "ServiceAccount", "")}
	rule.Spec.WorkloadSelectorTemplates = []string{"custom:{{ .Inputs.sa.metadata.labels.role }}", "custom:{{ .Inputs.sa.metadata.labels.role }}"}
	a, b := serviceAccount("team", "a"), serviceAccount("team", "b")
	a.Labels, b.Labels = map[string]string{"role": "app"}, map[string]string{"role": "app"}
	r, k8s, store, inputs := newIdentitySetTest(t, rule, a, b)
	r.reconcile(ctx)
	require.Len(t, store.entries, 1)
	status := setStatus(t, k8s, rule.Name)
	require.Equal(t, 2, status.Stats.Combinations)
	require.Equal(t, 1, status.Stats.EntriesToSet)
	require.Zero(t, status.Stats.EntriesMasked)
	ids := store.ids()
	inputs.reverse = true
	r.reconcile(ctx)
	require.Equal(t, ids, store.ids())
	require.NoError(t, k8s.Delete(ctx, a))
	r.reconcile(ctx)
	require.Equal(t, ids, store.ids())
	updateTestObject(t, k8s, b, func() { b.Labels["role"] = "new" })
	r.reconcile(ctx)
	require.Len(t, store.entries, 1)
	require.NotEqual(t, ids, store.ids())
	require.NoError(t, k8s.Delete(ctx, b))
	r.reconcile(ctx)
	require.Empty(t, store.entries)
	require.NoError(t, k8s.Delete(ctx, rule))
	r.reconcile(ctx)
	require.Empty(t, inputs.active)
}

func TestIdentitySetFailuresAndRecovery(t *testing.T) {
	for _, failure := range []string{"discovery", "list", "watch", "template", "empty-join", "forward-join", "cluster-scope-join", "Pod", "Secret"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			rule := constantSet("set")
			rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("sa", "ServiceAccount", "")}
			r, k8s, store, inputs := newIdentitySetTest(t, rule, serviceAccount("team", "app"))
			r.reconcile(ctx)
			require.Len(t, store.entries, 1)
			ids := store.ids()
			switch failure {
			case "discovery":
				inputs.resolveErr = errors.New("discovery unavailable")
			case "list":
				inputs.listErr = errors.New("Forbidden: cannot list serviceaccounts")
			case "watch":
				inputs.healthErr = errors.New("watch disconnected")
			case "template":
				updateTestObject(t, k8s, rule, func() { rule.Spec.WorkloadSelectorTemplates = []string{"custom:{{ .PodMeta.Name }}"} })
			case "empty-join":
				updateTestObject(t, k8s, rule, func() { value := "{{ if false }}team{{ end }}"; rule.Spec.ForEach[0].NamespaceTemplate = &value })
			case "forward-join":
				updateTestObject(t, k8s, rule, func() { value := "{{ .Inputs.later.metadata.name }}"; rule.Spec.ForEach[0].NamespaceTemplate = &value })
			case "cluster-scope-join":
				updateTestObject(t, k8s, rule, func() { rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("ns", "Namespace", "team")} })
			case "Pod", "Secret":
				updateTestObject(t, k8s, rule, func() { rule.Spec.ForEach[0].Kind = failure })
			}
			r.reconcile(ctx)
			status := setStatus(t, k8s, rule.Name)
			condition := meta.FindStatusCondition(status.Conditions, "Ready")
			require.Equal(t, metav1.ConditionFalse, condition.Status)
			if failure == "discovery" || failure == "list" || failure == "watch" {
				require.Equal(t, "InputUnavailable", condition.Reason)
				require.Equal(t, ids, store.ids(), "incomplete snapshots must not collect existing entries")
			} else {
				require.Empty(t, store.entries)
			}
			inputs.resolveErr, inputs.listErr, inputs.healthErr = nil, nil, nil
			updateTestObject(t, k8s, rule, func() { rule.Spec = constantSet("unused").Spec })
			r.reconcile(ctx)
			require.Len(t, store.entries, 1)
			require.Empty(t, inputs.active)
			require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(setStatus(t, k8s, rule.Name).Conditions, "Ready").Status)
		})
	}
}

func TestIdentitySetServiceAccountMigrationAndRollback(t *testing.T) {
	ctx := context.Background()
	old := serviceAccountRule("legacy")
	var objects []client.Object
	objects = append(objects, old)
	for _, ns := range []string{"alpha", "beta"} {
		objects = append(objects, serviceAccountNamespace(ns, true))
		for i := range 3 {
			objects = append(objects, serviceAccount(ns, fmt.Sprintf("app-%d", i)))
		}
	}
	for i := range 4 {
		objects = append(objects, &spirev1alpha1.ClusterStaticEntry{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("bootstrap-%d", i), UID: types.UID(fmt.Sprintf("static-%d", i))},
			Spec:       spirev1alpha1.ClusterStaticEntrySpec{SPIFFEID: fmt.Sprintf("spiffe://example.org/bootstrap/%d", i), ParentID: "spiffe://example.org/root", Selectors: []string{fmt.Sprintf("bootstrap:id:%d", i)}},
		})
	}
	r, k8s, store := newServiceAccountTestReconciler(t, objects...)
	r.config.Reconcile.ClusterStaticEntries = true
	r.reconcile(ctx)
	require.Len(t, store.entries, 10)
	before := store.ids()
	baseline := make(map[string]string)
	for id, entry := range store.entries {
		baseline[id] = string(makeEntryKey(entry))
	}
	creates, updates, deletes := store.createCalls, store.updateCalls, store.deleteCalls
	check := func() {
		t.Helper()
		r.reconcile(ctx)
		require.Equal(t, before, store.ids())
		for id, entry := range store.entries {
			require.Equal(t, baseline[id], string(makeEntryKey(entry)))
		}
		require.Equal(t, creates, store.createCalls)
		require.Equal(t, updates, store.updateCalls)
		require.Equal(t, deletes, store.deleteCalls)
	}
	generic := constantSet("generic")
	generic.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	generic.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("ns", "Namespace", ""), setInput("sa", "ServiceAccount", "{{ .Inputs.ns.metadata.name }}")}
	generic.Spec.SPIFFEIDTemplate = "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}/sa/{{ .Inputs.sa.metadata.name }}"
	generic.Spec.ParentIDTemplate = old.Spec.ParentIDTemplate
	generic.Spec.WorkloadSelectorTemplates = []string{"k8s:ns:{{ .Inputs.ns.metadata.name }}", "k8s:sa:{{ .Inputs.sa.metadata.name }}"}
	require.NoError(t, k8s.Create(ctx, generic))
	r.config.Reconcile.ClusterSPIFFEIDSets = true
	inputs := &testSetInputs{client: k8s.Client}
	r.config.Inputs = inputs
	check()
	require.Equal(t, 6, setStatus(t, k8s, generic.Name).Stats.EntriesMasked)
	require.Zero(t, setStatus(t, k8s, generic.Name).Stats.EntriesToSet)
	require.NoError(t, k8s.Delete(ctx, old))
	check()
	require.Equal(t, 6, setStatus(t, k8s, generic.Name).Stats.EntriesToSet)
	require.Zero(t, setStatus(t, k8s, generic.Name).Stats.EntriesMasked)
	inputs.Close()
	r.config.Inputs = &testSetInputs{client: k8s.Client}
	r.config.Reconcile.ClusterServiceAccountSPIFFEIDs = false
	r = &entryReconciler{config: r.config, promCounter: metrics.PromCounters}
	check()

	rollback := serviceAccountRule("rollback")
	rollback.CreationTimestamp = metav1.NewTime(time.Unix(3, 0))
	require.NoError(t, k8s.Create(ctx, rollback))
	r.config.Reconcile.ClusterServiceAccountSPIFFEIDs = true
	check()
	require.Equal(t, 6, serviceAccountStatus(t, k8s, rollback.Name).Stats.EntriesMasked)
	require.NoError(t, k8s.Delete(ctx, generic))
	check()
	require.Equal(t, 6, serviceAccountStatus(t, k8s, rollback.Name).Stats.EntriesToSet)
	r.config.Inputs.Close()
	r.config.Inputs = nil
	r.config.Reconcile.ClusterSPIFFEIDSets = false
	r = &entryReconciler{config: r.config, promCounter: metrics.PromCounters}
	check()
}

func TestIdentitySetSPIREFailuresReportAndRetry(t *testing.T) {
	ctx := context.Background()
	rule := constantSet("set")
	r, k8s, store, _ := newIdentitySetTest(t, rule)
	store.createErr = errors.New("SPIRE unavailable")
	r.reconcile(ctx)
	status := setStatus(t, k8s, rule.Name)
	require.Equal(t, "SPIREAPIError", meta.FindStatusCondition(status.Conditions, "Ready").Reason)
	require.Equal(t, 1, status.Stats.EntryFailures)
	store.createErr = nil
	r.reconcile(ctx)
	require.Len(t, store.entries, 1)
	store.listErr = errors.New("SPIRE list unavailable")
	r.reconcile(ctx)
	require.Len(t, store.entries, 1)
	require.Equal(t, "SPIREAPIError", meta.FindStatusCondition(setStatus(t, k8s, rule.Name).Conditions, "Ready").Reason)
	store.listErr = nil
	updateTestObject(t, k8s, rule, func() { rule.Spec.WorkloadSelectorTemplates = nil })
	store.deleteErr = errors.New("SPIRE delete unavailable")
	r.reconcile(ctx)
	require.Len(t, store.entries, 1)
	require.Equal(t, "SPIREAPIError", meta.FindStatusCondition(setStatus(t, k8s, rule.Name).Conditions, "Ready").Reason)
	store.deleteErr = nil
	r.reconcile(ctx)
	require.Empty(t, store.entries)
	require.Equal(t, "InvalidSpec", meta.FindStatusCondition(setStatus(t, k8s, rule.Name).Conditions, "Ready").Reason)
}

func constantSet(name string) *spirev1alpha1.ClusterSPIFFEIDSet {
	return &spirev1alpha1.ClusterSPIFFEIDSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name), Generation: 1, CreationTimestamp: metav1.NewTime(time.Unix(1, 0))},
		Spec:       spirev1alpha1.ClusterSPIFFEIDSetSpec{SPIFFEIDTemplate: "spiffe://{{ .TrustDomain }}/constant", ParentIDTemplate: "spiffe://{{ .TrustDomain }}/ztunnel", WorkloadSelectorTemplates: []string{"custom:constant"}},
	}
}

func setInput(alias, kind, namespace string) spirev1alpha1.SPIFFEIDSetInput {
	input := spirev1alpha1.SPIFFEIDSetInput{SPIFFEIDSetKind: spirev1alpha1.SPIFFEIDSetKind{APIVersion: "v1", Kind: kind}, Alias: alias}
	if namespace != "" {
		input.NamespaceTemplate = &namespace
	}
	return input
}

func newIdentitySetTest(t *testing.T, objects ...client.Object) (*entryReconciler, *serviceAccountTestClient, *serviceAccountEntryClient, *testSetInputs) {
	t.Helper()
	r, k8s, store := newServiceAccountTestReconciler(t, objects...)
	inputs := &testSetInputs{client: k8s.Client}
	r.config.Reconcile = spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDSets: true}
	r.config.Inputs = inputs
	return r, k8s, store, inputs
}

func setStatus(t *testing.T, c *serviceAccountTestClient, name string) spirev1alpha1.ClusterSPIFFEIDSetStatus {
	t.Helper()
	var obj spirev1alpha1.ClusterSPIFFEIDSet
	require.NoError(t, c.Client.Get(context.Background(), types.NamespacedName{Name: name}, &obj))
	return obj.Status
}

type testSetInputs struct {
	client                         client.Client
	active                         []k8sinput.Kind
	namespaces                     map[string][]string
	resolveErr, listErr, healthErr error
	reverse                        bool
}

func (s *testSetInputs) Resolve(gvk schema.GroupVersionKind) (k8sinput.Kind, error) {
	if s.resolveErr != nil {
		return k8sinput.Kind{}, s.resolveErr
	}
	return k8sinput.Kind{GVK: gvk, Namespaced: gvk.Kind != "Namespace"}, nil
}

func (s *testSetInputs) Sync(_ context.Context, kinds []k8sinput.Kind) error {
	s.active = kinds
	s.namespaces = make(map[string][]string)
	return nil
}

func (s *testSetInputs) Check(schema.GroupVersionKind) error { return s.healthErr }

func (s *testSetInputs) Retain(kinds []schema.GroupVersionKind) {
	s.active = slices.DeleteFunc(s.active, func(kind k8sinput.Kind) bool { return !slices.Contains(kinds, kind.GVK) })
}

func (s *testSetInputs) List(gvk schema.GroupVersionKind, namespace string, selector labels.Selector) ([]unstructured.Unstructured, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.namespaces[gvk.Kind] = append(s.namespaces[gvk.Kind], namespace)
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := s.client.List(context.Background(), list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	if s.reverse {
		slices.Reverse(list.Items)
	}
	return list.Items, nil
}

func (s *testSetInputs) activeKinds() []string {
	var kinds []string
	for _, kind := range s.active {
		kinds = append(kinds, kind.GVK.Kind)
	}
	return kinds
}

func (*testSetInputs) SetTrigger(func()) {}
func (s *testSetInputs) Close()          { s.active = nil }
