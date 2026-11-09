package spireentry

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/pkg/k8sinput"
	"github.com/spiffe/spire-controller-manager/pkg/metrics"
	"github.com/spiffe/spire-controller-manager/pkg/spireapi"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIdentitySetIterationDomains(t *testing.T) {
	for _, tt := range []struct {
		name, identity string
		inputs         []spirev1alpha1.SPIFFEIDSetInput
		selectors      []string
		want           int
		kinds          []string
	}{
		{"constant", "spiffe://{{ .TrustDomain }}/constant", nil, []string{"custom:{{ .ClusterName }}"}, 1, nil},
		{"empty inputs", "spiffe://{{ .TrustDomain }}/constant", []spirev1alpha1.SPIFFEIDSetInput{}, []string{"custom:constant"}, 1, nil},
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
				setNamespace("alpha"), setNamespace("beta"),
				setServiceAccount("alpha", "one"), setServiceAccount("alpha", "two"), setServiceAccount("beta", "one"),
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "identity", Namespace: "alpha"}, Data: map[string]string{"identity": "configmap", "selector": "value:with:colons"}})
			r.reconcile(context.Background())
			require.Len(t, store.entries, tt.want)
			require.ElementsMatch(t, tt.kinds, inputs.activeKinds())
			require.Equal(t, tt.want, setStatus(t, k8s, rule.Name).Stats.Combinations)
			for _, entry := range store.entries {
				require.Len(t, entry.Selectors, len(tt.selectors), "only explicit selectors are present")
				for _, selector := range entry.Selectors {
					require.NotContains(t, selector.Value, "pod-uid")
				}
			}
			if tt.name == "namespace join" {
				require.Equal(t, []string{"alpha", "beta"}, inputs.namespaces["ServiceAccount"], "joins must query the namespace index")
			}
			if tt.name == "constant" {
				r.config.Inputs = nil
				r.reconcile(context.Background())
				require.Len(t, store.entries, 1, "constants do not require any input source")
			}
		})
	}
}

func TestIdentitySetCoalescingAndInputLifecycle(t *testing.T) {
	ctx := context.Background()
	rule := constantSet("set")
	rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("sa", "ServiceAccount", "")}
	rule.Spec.WorkloadSelectorTemplates = []string{"custom:{{ .Inputs.sa.metadata.labels.role }}", "custom:{{ .Inputs.sa.metadata.labels.role }}"}
	a, b := setServiceAccount("team", "a"), setServiceAccount("team", "b")
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
	other := rule.DeepCopy()
	other.ObjectMeta = metav1.ObjectMeta{Name: "other", UID: "other", CreationTimestamp: metav1.NewTime(time.Unix(2, 0))}
	require.NoError(t, k8s.Create(ctx, other))
	r.reconcile(ctx)
	require.Equal(t, ids, store.ids())
	require.Equal(t, 1, setStatus(t, k8s, other.Name).Stats.EntriesMasked)
	require.NoError(t, k8s.Delete(ctx, rule))
	r.reconcile(ctx)
	require.Equal(t, ids, store.ids())
	require.Equal(t, 1, setStatus(t, k8s, other.Name).Stats.EntriesToSet)
	require.NoError(t, k8s.Delete(ctx, a))
	r.reconcile(ctx)
	require.Equal(t, ids, store.ids())
	updateSetObject(t, k8s, b, func() { b.Labels["role"] = "new" })
	r.reconcile(ctx)
	require.Len(t, store.entries, 1)
	require.NotEqual(t, ids, store.ids())
	require.NoError(t, k8s.Delete(ctx, b))
	r.reconcile(ctx)
	require.Empty(t, store.entries)
	require.NoError(t, k8s.Delete(ctx, other))
	r.reconcile(ctx)
	require.Empty(t, inputs.active)
}

func TestIdentitySetFailuresAndRecovery(t *testing.T) {
	for _, failure := range []string{"discovery", "list", "watch", "template", "empty-join", "invalid-join", "forward-join", "cluster-scope-join", "Pod", "Secret"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			rule := constantSet("set")
			rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("sa", "ServiceAccount", "")}
			r, k8s, store, inputs := newIdentitySetTest(t, rule, setServiceAccount("team", "app"))
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
				updateSetObject(t, k8s, rule, func() { rule.Spec.WorkloadSelectorTemplates = []string{"custom:{{ .PodMeta.Name }}"} })
			case "empty-join":
				updateSetObject(t, k8s, rule, func() { value := "{{ if false }}team{{ end }}"; rule.Spec.ForEach[0].NamespaceTemplate = &value })
			case "invalid-join":
				updateSetObject(t, k8s, rule, func() { value := "not/a/namespace"; rule.Spec.ForEach[0].NamespaceTemplate = &value })
			case "forward-join":
				updateSetObject(t, k8s, rule, func() { value := "{{ .Inputs.later.metadata.name }}"; rule.Spec.ForEach[0].NamespaceTemplate = &value })
			case "cluster-scope-join":
				updateSetObject(t, k8s, rule, func() { rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("ns", "Namespace", "team")} })
			case "Pod", "Secret":
				updateSetObject(t, k8s, rule, func() { rule.Spec.ForEach[0].Kind = failure })
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
			r.reconcile(ctx)
			require.Equal(t, status, setStatus(t, k8s, rule.Name), "unchanged status must not churn transition time")
			inputs.resolveErr, inputs.listErr, inputs.healthErr = nil, nil, nil
			updateSetObject(t, k8s, rule, func() { rule.Spec = constantSet("unused").Spec })
			r.reconcile(ctx)
			require.Len(t, store.entries, 1)
			require.Empty(t, inputs.active)
			require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(setStatus(t, k8s, rule.Name).Conditions, "Ready").Status)
		})
	}
}

func TestIdentitySetPreservesBootstrapAndRestarts(t *testing.T) {
	ctx := context.Background()
	rule := constantSet("workloads")
	rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("ns", "Namespace", ""), setInput("sa", "ServiceAccount", "{{ .Inputs.ns.metadata.name }}")}
	rule.Spec.SPIFFEIDTemplate = "spiffe://{{ .TrustDomain }}/ns/{{ .Inputs.ns.metadata.name }}/sa/{{ .Inputs.sa.metadata.name }}"
	rule.Spec.WorkloadSelectorTemplates = []string{"k8s:ns:{{ .Inputs.ns.metadata.name }}", "k8s:sa:{{ .Inputs.sa.metadata.name }}"}
	var objects []client.Object
	for _, ns := range []string{"alpha", "beta"} {
		objects = append(objects, setNamespace(ns))
		for i := range 3 {
			objects = append(objects, setServiceAccount(ns, fmt.Sprintf("app-%d", i)))
		}
	}
	for i := range 4 {
		objects = append(objects, &spirev1alpha1.ClusterStaticEntry{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("bootstrap-%d", i), UID: types.UID(fmt.Sprintf("static-%d", i))},
			Spec:       spirev1alpha1.ClusterStaticEntrySpec{SPIFFEID: fmt.Sprintf("spiffe://example.org/bootstrap/%d", i), ParentID: "spiffe://example.org/root", Selectors: []string{fmt.Sprintf("bootstrap:id:%d", i)}},
		})
	}
	r, k8s, store, inputs := newIdentitySetTest(t, objects...)
	r.config.Reconcile = spirev1alpha1.ReconcileConfig{ClusterStaticEntries: true}
	r.reconcile(ctx)
	require.Len(t, store.entries, 4)
	bootstrap := store.snapshot()
	require.NoError(t, k8s.Create(ctx, rule))
	r.config.Reconcile.ClusterSPIFFEIDSets = true
	r.reconcile(ctx)
	require.Len(t, store.entries, 10)
	all := store.snapshot()
	creates, updates, deletes := store.createCalls, store.updateCalls, store.deleteCalls
	r = &entryReconciler{config: r.config, promCounter: metrics.PromCounters}
	r.reconcile(ctx)
	require.Equal(t, all, store.snapshot())
	inputs.healthErr = errors.New("watch unavailable")
	r.reconcile(ctx)
	require.Equal(t, all, store.snapshot())
	require.Equal(t, creates, store.createCalls)
	require.Equal(t, updates, store.updateCalls)
	require.Equal(t, deletes, store.deleteCalls)
	inputs.healthErr = nil
	require.NoError(t, k8s.Delete(ctx, rule))
	r.reconcile(ctx)
	require.Equal(t, bootstrap, store.snapshot(), "source deletion must preserve existing static registrations and their IDs")
	require.NoError(t, k8s.Create(ctx, constantSet("constant")))
	r.reconcile(ctx)
	retained := store.snapshot()
	r.config.Reconcile.ClusterSPIFFEIDSets = false
	r.reconcile(ctx)
	require.Equal(t, retained, store.snapshot(), "disabling Sets must not collect scoped registrations")
}

func TestIdentitySetFiltersAndOwnership(t *testing.T) {
	for _, selection := range []string{"different class", "classless", "watch classless", "ignored namespace", "terminating input", "terminating rule"} {
		t.Run(selection, func(t *testing.T) {
			rule := constantSet("set")
			rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("sa", "ServiceAccount", "")}
			sa := setServiceAccount("team", "app")
			switch selection {
			case "different class":
				rule.Spec.ClassName = "another"
			case "terminating input":
				now := metav1.Now()
				sa.DeletionTimestamp, sa.Finalizers = &now, []string{"example.org/hold"}
			case "terminating rule":
				now := metav1.Now()
				rule.DeletionTimestamp, rule.Finalizers = &now, []string{"example.org/hold"}
			}
			r, _, store, _ := newIdentitySetTest(t, rule, sa)
			switch selection {
			case "classless", "watch classless":
				r.config.ClassName = "selected"
				r.config.WatchClassless = selection == "watch classless"
			case "ignored namespace":
				r.config.IgnoreNamespaces = []*regexp.Regexp{regexp.MustCompile("^team$")}
			}
			r.reconcile(context.Background())
			if selection == "watch classless" {
				require.Len(t, store.entries, 1)
			} else {
				require.Empty(t, store.entries)
			}
		})
	}

	r, _, store, _ := newIdentitySetTest(t, constantSet("set"))
	r.reconcile(context.Background())
	owned := store.snapshot()
	for _, id := range []string{"bootstrap", "test.random", scopedEntryIDPrefix("test."+identitySetEntryIDMarker, "example.org", "another-cluster", "") + "entry"} {
		store.entries[id] = spireapi.Entry{ID: id, SPIFFEID: spiffeid.RequireFromString("spiffe://example.org/" + id), ParentID: spiffeid.RequireFromString("spiffe://example.org/root")}
	}
	all := store.snapshot()
	r.reconcile(context.Background())
	require.Equal(t, all, store.snapshot(), "Set-only reconciliation must ignore other sources and scopes")
	for id := range owned {
		delete(store.entries, id)
	}
	r.config.Reconcile.ClusterSPIFFEIDSets = false
	r.config.Reconcile.ClusterStaticEntries = true
	r.reconcile(context.Background())
	require.NotContains(t, store.entries, "test.random", "stock prefix cleanup must still work when Sets are disabled")
	require.Contains(t, store.entries, "bootstrap", "stock prefix isolation must remain unchanged")
}

func TestIdentitySetSPIREFailuresReportAndRetry(t *testing.T) {
	for _, failure := range []string{"list", "create", "create-status", "missing-create-results", "lost-create-response", "update", "delete", "delete-status"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			rule := constantSet("set")
			r, k8s, store, _ := newIdentitySetTest(t, rule)
			if failure == "update" || failure == "delete" || failure == "delete-status" {
				r.reconcile(ctx)
				require.Len(t, store.entries, 1)
			}
			switch failure {
			case "list":
				store.listErr = errors.New("SPIRE list unavailable")
			case "create":
				store.createErr = errors.New("SPIRE create unavailable")
			case "create-status":
				store.createStatus = codes.PermissionDenied
			case "missing-create-results":
				store.malformedCreate = true
			case "lost-create-response":
				store.lostCreateResponse = true
			case "update":
				for id, entry := range store.entries {
					entry.X509SVIDTTL = time.Hour
					store.entries[id] = entry
				}
				store.updateErr = errors.New("SPIRE update unavailable")
			case "delete", "delete-status":
				updateSetObject(t, k8s, rule, func() { rule.Spec.WorkloadSelectorTemplates = nil })
				if failure == "delete" {
					store.deleteErr = errors.New("SPIRE delete unavailable")
				} else {
					store.deleteStatus = codes.PermissionDenied
				}
			}
			r.reconcile(ctx)
			require.Equal(t, "SPIREAPIError", meta.FindStatusCondition(setStatus(t, k8s, rule.Name).Conditions, "Ready").Reason)
			store.listErr, store.createErr, store.updateErr, store.deleteErr = nil, nil, nil, nil
			store.createStatus, store.deleteStatus = codes.OK, codes.OK
			store.malformedCreate, store.lostCreateResponse = false, false
			r.reconcile(ctx)
			if failure == "delete" || failure == "delete-status" {
				require.Empty(t, store.entries)
				require.Equal(t, "InvalidSpec", meta.FindStatusCondition(setStatus(t, k8s, rule.Name).Conditions, "Ready").Reason)
			} else {
				require.Len(t, store.entries, 1)
				require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(setStatus(t, k8s, rule.Name).Conditions, "Ready").Status)
			}
		})
	}
}

func constantSet(name string) *spirev1alpha1.ClusterSPIFFEIDSet {
	return &spirev1alpha1.ClusterSPIFFEIDSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name), Generation: 1, CreationTimestamp: metav1.NewTime(time.Unix(1, 0))},
		Spec:       spirev1alpha1.ClusterSPIFFEIDSetSpec{SPIFFEIDTemplate: "spiffe://{{ .TrustDomain }}/constant", ParentIDTemplate: "spiffe://{{ .TrustDomain }}/parent", WorkloadSelectorTemplates: []string{"custom:constant"}},
	}
}

func setInput(alias, kind, namespace string) spirev1alpha1.SPIFFEIDSetInput {
	input := spirev1alpha1.SPIFFEIDSetInput{SPIFFEIDSetKind: spirev1alpha1.SPIFFEIDSetKind{APIVersion: "v1", Kind: kind}, Alias: alias}
	if namespace != "" {
		input.NamespaceTemplate = &namespace
	}
	return input
}

func setNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func setServiceAccount(namespace, name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

func newIdentitySetTest(t *testing.T, objects ...client.Object) (*entryReconciler, *identitySetTestClient, *identitySetEntryClient, *testSetInputs) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))
	k8s := &identitySetTestClient{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
			WithStatusSubresource(&spirev1alpha1.ClusterSPIFFEIDSet{}, &spirev1alpha1.ClusterStaticEntry{}, &spirev1alpha1.ClusterSPIFFEID{}).Build(),
		t: t,
	}
	store := &identitySetEntryClient{entries: map[string]spireapi.Entry{}}
	inputs := &testSetInputs{client: k8s}
	r := &entryReconciler{
		config: ReconcilerConfig{
			TrustDomain: spiffeid.RequireTrustDomainFromString("example.org"), ClusterName: "test",
			K8sClient: k8s, Inputs: inputs, EntryClient: store, EntryIDPrefix: "test.",
			Reconcile: spirev1alpha1.ReconcileConfig{ClusterSPIFFEIDSets: true},
		},
		promCounter: metrics.PromCounters,
	}
	return r, k8s, store, inputs
}

func setStatus(t *testing.T, c client.Client, name string) spirev1alpha1.ClusterSPIFFEIDSetStatus {
	t.Helper()
	var obj spirev1alpha1.ClusterSPIFFEIDSet
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name}, &obj))
	return obj.Status
}

func updateSetObject(t *testing.T, c client.Client, object client.Object, change func()) {
	t.Helper()
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(object), object))
	change()
	require.NoError(t, c.Update(context.Background(), object))
}

type identitySetTestClient struct {
	client.Client
	t *testing.T
}

func (c *identitySetTestClient) List(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	switch obj.(type) {
	case *corev1.PodList, *corev1.NodeList, *corev1.EndpointsList:
		c.t.Errorf("unexpected list of %T on Set-only path", obj)
		return fmt.Errorf("forbidden list of %T", obj)
	}
	return c.Client.List(ctx, obj, opts...)
}

type identitySetEntryClient struct {
	entries                                  map[string]spireapi.Entry
	listErr, createErr, updateErr, deleteErr error
	createStatus, deleteStatus               codes.Code
	malformedCreate, lostCreateResponse      bool
	createCalls, updateCalls, deleteCalls    int
}

func (*identitySetEntryClient) GetUnsupportedFields(context.Context, string) (map[spireapi.Field]struct{}, error) {
	return nil, nil
}

func (c *identitySetEntryClient) ListEntries(context.Context) ([]spireapi.Entry, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	var entries []spireapi.Entry
	for _, entry := range c.entries {
		entries = append(entries, entry)
	}
	return entries, nil
}

func (c *identitySetEntryClient) CreateEntries(_ context.Context, entries []spireapi.Entry) ([]spireapi.Status, error) {
	c.createCalls++
	if c.createErr != nil {
		return nil, c.createErr
	}
	var statuses []spireapi.Status
	for _, entry := range entries {
		code := c.createStatus
		for _, current := range c.entries {
			if makeEntryKey(entry) == makeEntryKey(current) {
				code = codes.AlreadyExists
			}
		}
		if code == codes.OK {
			if entry.ID == "" {
				entry.ID = uuid.NewString()
			}
			c.entries[entry.ID] = entry
		}
		statuses = append(statuses, spireapi.Status{Code: code, Message: code.String()})
	}
	if c.lostCreateResponse {
		return nil, errors.New("create committed but response was lost")
	}
	if c.malformedCreate {
		return nil, nil
	}
	return statuses, nil
}

func (c *identitySetEntryClient) UpdateEntries(_ context.Context, entries []spireapi.Entry) ([]spireapi.Status, error) {
	c.updateCalls++
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	for _, entry := range entries {
		c.entries[entry.ID] = entry
	}
	return make([]spireapi.Status, len(entries)), nil
}

func (c *identitySetEntryClient) DeleteEntries(_ context.Context, ids []string) ([]spireapi.Status, error) {
	c.deleteCalls++
	if c.deleteErr != nil {
		return nil, c.deleteErr
	}
	statuses := make([]spireapi.Status, len(ids))
	for i, id := range ids {
		statuses[i] = spireapi.Status{Code: c.deleteStatus, Message: c.deleteStatus.String()}
		if c.deleteStatus == codes.OK {
			delete(c.entries, id)
		}
	}
	return statuses, nil
}

func (c *identitySetEntryClient) ids() []string {
	ids := make([]string, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (c *identitySetEntryClient) snapshot() map[string]spireapi.Entry {
	out := make(map[string]spireapi.Entry, len(c.entries))
	for id, entry := range c.entries {
		out[id] = entry
	}
	return out
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
