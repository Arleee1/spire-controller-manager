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
	"github.com/spiffe/spire-controller-manager/pkg/metrics"
	"github.com/spiffe/spire-controller-manager/pkg/spireapi"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const enrollmentLabel = "io.cilium/mtls-enabled"

func TestServiceAccountEntryRendering(t *testing.T) {
	for _, tt := range []struct {
		name, identity, parent, wantID, wantParent, err string
	}{
		{name: "pair identity", wantID: "spiffe://example.org/ns/team/sa/app", wantParent: "spiffe://example.org/ztunnel"},
		{name: "shared agent alias", parent: "spiffe://{{ .TrustDomain }}/ns/spire/sa/spire-agent", wantID: "spiffe://example.org/ns/team/sa/app", wantParent: "spiffe://example.org/ns/spire/sa/spire-agent"},
		{name: "metadata", identity: "spiffe://{{ .TrustDomain }}/{{ .ClusterName }}/{{ .NamespaceMeta.Labels.team }}/{{ .ServiceAccountMeta.Labels.role }}", wantID: "spiffe://example.org/test/payments/frontend", wantParent: "spiffe://example.org/ztunnel"},
		{name: "no Pod fields", identity: "spiffe://{{ .TrustDomain }}/{{ .PodMeta.Name }}", err: "can't evaluate field PodMeta"},
		{name: "no Node fields", parent: "spiffe://{{ .TrustDomain }}/{{ .NodeMeta.UID }}", err: "can't evaluate field NodeMeta"},
		{name: "missing metadata", identity: "spiffe://{{ .TrustDomain }}/{{ .ServiceAccountMeta.Labels.missing }}", err: "map has no entry"},
		{name: "invalid ID", identity: "https://example.org/workload", err: "invalid SPIFFE ID"},
		{name: "wrong identity trust domain", identity: "spiffe://other.org/workload", err: "expected trust domain"},
		{name: "wrong parent trust domain", parent: "spiffe://other.org/parent", err: "expected trust domain"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rule := serviceAccountRule("identities")
			if tt.identity != "" {
				rule.Spec.SPIFFEIDTemplate = tt.identity
			}
			if tt.parent != "" {
				rule.Spec.ParentIDTemplate = tt.parent
			}
			spec, err := spirev1alpha1.ParseClusterServiceAccountSPIFFEIDSpec(&rule.Spec)
			require.NoError(t, err)
			ns := serviceAccountNamespace("team", true)
			ns.Labels["team"] = "payments"
			sa := serviceAccount("team", "app")
			sa.Labels = map[string]string{"role": "frontend"}
			entry, err := renderServiceAccountEntry(spec, ns, sa, spiffeid.RequireTrustDomainFromString(trustDomain), clusterName)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantID, entry.SPIFFEID.String())
			require.Equal(t, tt.wantParent, entry.ParentID.String())
			require.Equal(t, []spireapi.Selector{{Type: "k8s", Value: "ns:team"}, {Type: "k8s", Value: "sa:app"}}, entry.Selectors)
			require.False(t, entry.Admin)
			require.False(t, entry.Downstream)
		})
	}
}

func TestServiceAccountEntryCountIsIndependentOfPods(t *testing.T) {
	for _, podCount := range []int{0, 1, 1000} {
		t.Run(fmt.Sprint(podCount), func(t *testing.T) {
			objects := []client.Object{
				serviceAccountRule("identities"), serviceAccountNamespace("team", true),
				serviceAccount("team", "app"), serviceAccount("team", "idle"),
				serviceAccountNamespace("not-enrolled", false), serviceAccount("not-enrolled", "app"),
			}
			for i := range podCount {
				objects = append(objects, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), Namespace: "team", UID: types.UID(fmt.Sprint(i))},
					Spec:       corev1.PodSpec{ServiceAccountName: "app", NodeName: "unused"},
				})
			}
			r, k8s, store := newServiceAccountTestReconciler(t, objects...)
			r.reconcile(context.Background())
			require.Len(t, store.entries, 2)
			require.Equal(t, 1, k8s.lists["*v1.NamespaceList"])
			require.Equal(t, 1, k8s.lists["*v1.ServiceAccountList"])
			require.Zero(t, k8s.lists["*v1.PodList"])
			require.Zero(t, k8s.lists["*v1.NodeList"])
			require.Zero(t, k8s.lists["*v1.EndpointsList"])
			require.Empty(t, k8s.gets)
			status := serviceAccountStatus(t, k8s, "identities")
			require.Equal(t, 2, status.Stats.ServiceAccountsSelected)
			require.Equal(t, 2, status.Stats.EntriesToSet)
			require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(status.Conditions, "Ready").Status)
		})
	}
}

func TestServiceAccountLifecyclePreservesStaticEntries(t *testing.T) {
	ctx := context.Background()
	rule := serviceAccountRule("identities")
	ns := serviceAccountNamespace("team", true)
	objects := []client.Object{rule, ns, serviceAccount("team", "app"), serviceAccount("team", "idle")}
	for i := range 4 {
		objects = append(objects, &spirev1alpha1.ClusterStaticEntry{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("bootstrap-%d", i), UID: types.UID(fmt.Sprintf("static-%d", i))},
			Spec: spirev1alpha1.ClusterStaticEntrySpec{
				SPIFFEID: fmt.Sprintf("spiffe://example.org/bootstrap/%d", i),
				ParentID: "spiffe://example.org/root", Selectors: []string{fmt.Sprintf("bootstrap:id:%d", i)},
			},
		})
	}
	r, k8s, store := newServiceAccountTestReconciler(t, objects...)
	r.config.Reconcile.ClusterStaticEntries = true
	foreign := spireapi.Entry{
		ID: "other-manager.untouched", SPIFFEID: spiffeid.RequireFromString("spiffe://example.org/unrelated"),
		ParentID: spiffeid.RequireFromString("spiffe://example.org/root"), Selectors: []spireapi.Selector{{Type: "other", Value: "workload"}},
	}
	store.entries[foreign.ID] = foreign
	store.entries["poc.old-pod"] = spireapi.Entry{
		ID: "poc.old-pod", SPIFFEID: spiffeid.RequireFromString("spiffe://example.org/ns/team/sa/app"),
		ParentID:  spiffeid.RequireFromString("spiffe://example.org/spire/agent/k8s_psat/test/old-node"),
		Selectors: []spireapi.Selector{{Type: "k8s", Value: "pod-uid:old-pod"}},
	}
	check := func(wantAccounts int) {
		t.Helper()
		r.reconcile(ctx)
		require.Len(t, store.entries, wantAccounts+5)
		require.Equal(t, foreign, store.entries[foreign.ID])
		for i := range 4 {
			require.True(t, store.hasSPIFFEID(fmt.Sprintf("spiffe://example.org/bootstrap/%d", i)))
		}
		require.Zero(t, k8s.lists["*v1.PodList"])
	}
	check(2)
	require.NotContains(t, store.entries, "poc.old-pod", "disabled legacy Pod declarations must be collected within the manager prefix")
	initialIDs := store.ids()
	createCalls := store.createCalls
	r = &entryReconciler{config: r.config, promCounter: metrics.PromCounters}
	check(2)
	require.Equal(t, initialIDs, store.ids())
	require.Equal(t, createCalls, store.createCalls, "restart must reuse stored entries")

	updateTestObject(t, k8s, ns, func() { ns.Labels[enrollmentLabel] = "false" })
	check(0)
	updateTestObject(t, k8s, ns, func() { ns.Labels[enrollmentLabel] = "true" })
	check(2)
	require.Equal(t, initialIDs, store.ids(), "reenrollment uses stable IDs")

	extra := serviceAccount("team", "extra")
	require.NoError(t, k8s.Create(ctx, extra))
	check(3)
	require.NoError(t, k8s.Delete(ctx, extra))
	check(2)

	updateTestObject(t, k8s, rule, func() {
		rule.Generation++
		rule.Spec.ServiceAccountSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"selected": "true"}}
	})
	check(0)
	app := serviceAccount("team", "app")
	updateTestObject(t, k8s, app, func() { app.Labels = map[string]string{"selected": "true"} })
	check(1)
	updateTestObject(t, k8s, app, func() { app.Labels["selected"] = "false" })
	check(0)
	updateTestObject(t, k8s, rule, func() {
		rule.Generation++
		rule.Spec.ServiceAccountSelector = nil
		rule.Spec.ParentIDTemplate = "spiffe://{{ .TrustDomain }}/new-parent"
	})
	check(2)
	require.NotEqual(t, initialIDs, store.ids())
	for _, entry := range store.entries {
		if len(entry.Selectors) == 2 {
			require.Equal(t, "spiffe://example.org/new-parent", entry.ParentID.String())
		}
	}

	secondNS := serviceAccountNamespace("second", true)
	require.NoError(t, k8s.Create(ctx, secondNS))
	require.NoError(t, k8s.Create(ctx, serviceAccount("second", "app")))
	check(3)
	require.NoError(t, k8s.Delete(ctx, secondNS))
	check(2)
	require.NoError(t, k8s.Delete(ctx, app))
	check(1)
	require.NoError(t, k8s.Delete(ctx, rule))
	check(0)
	require.NoError(t, k8s.Create(ctx, serviceAccountRule("replacement")))
	check(1)
}

func TestServiceAccountSelectionClassAndOverlap(t *testing.T) {
	ctx := context.Background()
	older := serviceAccountRule("older")
	older.Spec.ClassName = "poc"
	newer := serviceAccountRule("newer")
	newer.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	ignored := serviceAccountRule("other-class")
	ignored.Spec.ClassName = "other"
	ignored.Spec.SPIFFEIDTemplate = "spiffe://{{ .TrustDomain }}/other"
	r, k8s, store := newServiceAccountTestReconciler(t,
		older, newer, ignored,
		serviceAccountNamespace("team", true), serviceAccount("team", "app"),
		serviceAccountNamespace("ignored", true), serviceAccount("ignored", "app"))
	r.config.ClassName = "poc"
	r.config.IgnoreNamespaces = []*regexp.Regexp{regexp.MustCompile("^ignored$")}
	r.reconcile(ctx)
	require.Len(t, store.entries, 1)
	require.Empty(t, serviceAccountStatus(t, k8s, "newer").Conditions)
	require.Empty(t, serviceAccountStatus(t, k8s, "other-class").Conditions)
	require.Equal(t, 1, serviceAccountStatus(t, k8s, "older").Stats.NamespacesIgnored)
	ids := store.ids()
	r.config.WatchClassless = true
	r.reconcile(ctx)
	require.Equal(t, ids, store.ids())
	require.Equal(t, 1, serviceAccountStatus(t, k8s, "newer").Stats.EntriesMasked)
	require.Equal(t, 1, serviceAccountStatus(t, k8s, "older").Stats.EntriesToSet)
	require.NoError(t, k8s.Delete(ctx, older))
	r.reconcile(ctx)
	require.Equal(t, ids, store.ids(), "overlapping rule takes over without a new registration")
	require.Equal(t, 1, serviceAccountStatus(t, k8s, "newer").Stats.EntriesToSet)
	updateTestObject(t, k8s, newer, func() { newer.Spec.ClassName = "other" })
	r.reconcile(ctx)
	require.Empty(t, store.entries)
}

func TestServiceAccountOnlyScopeAndDisable(t *testing.T) {
	ctx := context.Background()
	rule := serviceAccountRule("identities")
	r, k8s, store := newServiceAccountTestReconciler(t, rule, serviceAccountNamespace("team", true), serviceAccount("team", "app"))
	r.reconcile(ctx)
	var own spireapi.Entry
	for _, entry := range store.entries {
		own = entry
	}
	foreign := own
	foreign.ID = "poc.unrelated"
	foreign.SPIFFEID = spiffeid.RequireFromString("spiffe://example.org/foreign")
	store.entries[foreign.ID] = foreign
	otherScope := *r
	otherScope.config.ClusterName = "other-cluster"
	other := foreign
	other.ID = otherScope.serviceAccountEntryIDPrefix() + "unrelated"
	store.entries[other.ID] = other
	r.config.Reconcile.ClusterServiceAccountSPIFFEIDs = false
	r.config.Reconcile.ClusterStaticEntries = true
	r.reconcile(ctx)
	require.Contains(t, store.entries, own.ID, "disabled SA reconciliation must not collect its scoped entries")
	require.Contains(t, store.entries, other.ID)
	r.config.Reconcile.ClusterServiceAccountSPIFFEIDs = true
	r.config.Reconcile.ClusterStaticEntries = false
	store.entries[foreign.ID] = foreign
	require.NoError(t, k8s.Delete(ctx, rule))
	r.reconcile(ctx)
	require.NotContains(t, store.entries, own.ID)
	require.Equal(t, foreign, store.entries[foreign.ID])
	require.Equal(t, other, store.entries[other.ID])
}

func TestServiceAccountFailuresAndRetries(t *testing.T) {
	for _, action := range []string{"namespace-list", "account-list", "rule-list", "spire-list", "create", "create-status", "create-response", "lost-create-response", "update", "delete", "delete-status"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			ns := serviceAccountNamespace("team", true)
			r, k8s, store := newServiceAccountTestReconciler(t, serviceAccountRule("identities"), ns, serviceAccount("team", "app"))
			createFailure := action == "create" || action == "create-status" || action == "create-response" || action == "lost-create-response"
			if !createFailure {
				r.reconcile(ctx)
			}
			originalIDs := store.ids()
			injected := errors.New("injected failure")
			switch action {
			case "namespace-list":
				k8s.listFailures["*v1.NamespaceList"] = injected
			case "account-list":
				k8s.listFailures["*v1.ServiceAccountList"] = injected
			case "rule-list":
				k8s.listFailures["*v1alpha1.ClusterServiceAccountSPIFFEIDList"] = injected
			case "spire-list":
				store.listErr = injected
			case "create":
				store.createErr = injected
			case "create-status":
				store.createStatus = codes.Unavailable
			case "create-response":
				store.malformedCreate = true
			case "lost-create-response":
				store.lostCreateResponse = true
			case "update":
				for id, entry := range store.entries {
					entry.Hint = "outdated"
					store.entries[id] = entry
				}
				store.updateErr = injected
			case "delete", "delete-status":
				updateTestObject(t, k8s, ns, func() { ns.Labels[enrollmentLabel] = "false" })
				if action == "delete" {
					store.deleteErr = injected
				} else {
					store.deleteStatus = codes.Unavailable
				}
			}
			r.reconcile(ctx)
			if !createFailure {
				require.Equal(t, originalIDs, store.ids(), "transient failures must not erase the last complete snapshot")
			}
			if action != "rule-list" {
				status := serviceAccountStatus(t, k8s, "identities")
				require.Equal(t, metav1.ConditionFalse, meta.FindStatusCondition(status.Conditions, "Ready").Status)
				previous := status
				r.reconcile(ctx)
				if action != "lost-create-response" && action != "create-response" {
					require.Equal(t, previous, serviceAccountStatus(t, k8s, "identities"), "repeated failures must not churn status")
				}
			}
			k8s.listFailures = map[string]error{}
			store.listErr, store.createErr, store.updateErr, store.deleteErr = nil, nil, nil, nil
			store.createStatus, store.deleteStatus = codes.OK, codes.OK
			store.malformedCreate, store.lostCreateResponse = false, false
			r = &entryReconciler{config: r.config, promCounter: metrics.PromCounters}
			r.reconcile(ctx)
			want := 1
			if action == "delete" || action == "delete-status" {
				want = 0
			}
			require.Len(t, store.entries, want)
			status := serviceAccountStatus(t, k8s, "identities")
			require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(status.Conditions, "Ready").Status)
			require.Zero(t, status.Stats.EntryFailures)
			if action == "lost-create-response" {
				require.Equal(t, 1, store.createCalls, "committed create with a lost response must not be repeated")
			}
		})
	}
}

func TestServiceAccountInvalidTemplateFailsClosed(t *testing.T) {
	for _, template := range []string{"{{", "spiffe://{{ .TrustDomain }}/{{ .PodMeta.UID }}", "spiffe://other.org/workload"} {
		t.Run(template, func(t *testing.T) {
			ctx := context.Background()
			rule := serviceAccountRule("identities")
			r, k8s, store := newServiceAccountTestReconciler(t, rule, serviceAccountNamespace("team", true), serviceAccount("team", "app"))
			r.reconcile(ctx)
			require.Len(t, store.entries, 1)
			updateTestObject(t, k8s, rule, func() { rule.Spec.SPIFFEIDTemplate = template; rule.Generation++ })
			r.reconcile(ctx)
			require.Empty(t, store.entries)
			status := serviceAccountStatus(t, k8s, "identities")
			require.Equal(t, rule.Generation, status.ObservedGeneration)
			condition := meta.FindStatusCondition(status.Conditions, "Ready")
			require.Equal(t, metav1.ConditionFalse, condition.Status)
			if template == "{{" {
				require.Equal(t, "InvalidSpec", condition.Reason)
			} else {
				require.Equal(t, "RenderFailed", condition.Reason)
				require.Equal(t, 1, status.Stats.EntryRenderFailures)
			}
			require.NotEmpty(t, condition.Message)
			r.reconcile(ctx)
			require.Equal(t, status, serviceAccountStatus(t, k8s, "identities"))
		})
	}
}

func TestServiceAccountAndLegacyPodEntriesCoexist(t *testing.T) {
	rule := serviceAccountRule("identities")
	r, k8s, store := newServiceAccountTestReconciler(t, rule, serviceAccountNamespace("team", true), serviceAccount("team", "app"),
		&spirev1alpha1.ClusterSPIFFEID{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy"},
			Spec:       spirev1alpha1.ClusterSPIFFEIDSpec{SPIFFEIDTemplate: "spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}"},
		},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "team", UID: "pod-uid"}, Spec: corev1.PodSpec{NodeName: "node", ServiceAccountName: "app"}},
	)
	k8s.allowPodAPIs = true
	r.config.Reconcile.ClusterSPIFFEIDs = true
	r.reconcile(context.Background())
	require.Len(t, store.entries, 2)
	var legacy, serviceAccounts int
	for _, entry := range store.entries {
		if len(entry.Selectors) == 1 {
			legacy++
			require.Equal(t, spireapi.Selector{Type: "k8s", Value: "pod-uid:pod-uid"}, entry.Selectors[0])
			require.Equal(t, "spiffe://example.org/spire/agent/k8s_psat/test/node-uid", entry.ParentID.String())
		} else {
			serviceAccounts++
			require.Equal(t, "spiffe://example.org/ztunnel", entry.ParentID.String())
		}
	}
	require.Equal(t, 1, legacy)
	require.Equal(t, 1, serviceAccounts)
}

func TestServiceAccountTerminatingObjectsAreExcluded(t *testing.T) {
	for _, kind := range []string{"namespace", "service-account", "rule"} {
		t.Run(kind, func(t *testing.T) {
			rule := serviceAccountRule("identities")
			ns := serviceAccountNamespace("team", true)
			sa := serviceAccount("team", "app")
			var obj client.Object
			switch kind {
			case "namespace":
				obj = ns
			case "service-account":
				obj = sa
			case "rule":
				obj = rule
			}
			deleting := metav1.Now()
			obj.SetDeletionTimestamp(&deleting)
			obj.SetFinalizers([]string{"test.example.org/hold"})
			r, _, store := newServiceAccountTestReconciler(t, rule, ns, sa)
			r.reconcile(context.Background())
			require.Empty(t, store.entries)
		})
	}
}

func TestServiceAccountDoesNotAdoptUnmanagedEntry(t *testing.T) {
	rule := serviceAccountRule("identities")
	ns := serviceAccountNamespace("team", true)
	sa := serviceAccount("team", "app")
	r, k8s, store := newServiceAccountTestReconciler(t, rule, ns, sa)
	spec, err := spirev1alpha1.ParseClusterServiceAccountSPIFFEIDSpec(&rule.Spec)
	require.NoError(t, err)
	entry, err := renderServiceAccountEntry(spec, ns, sa, r.config.TrustDomain, r.config.ClusterName)
	require.NoError(t, err)
	entry.ID = "unmanaged"
	store.entries[entry.ID] = *entry
	r.reconcile(context.Background())
	require.Equal(t, map[string]spireapi.Entry{"unmanaged": *entry}, store.entries)
	status := serviceAccountStatus(t, k8s, rule.Name)
	require.Equal(t, "SPIREAPIError", meta.FindStatusCondition(status.Conditions, "Ready").Reason)
	require.Equal(t, 1, status.Stats.EntryFailures)
}

func serviceAccountRule(name string) *spirev1alpha1.ClusterServiceAccountSPIFFEID {
	return &spirev1alpha1.ClusterServiceAccountSPIFFEID{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name), Generation: 1, CreationTimestamp: metav1.NewTime(time.Unix(1, 0))},
		Spec: spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec{
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{enrollmentLabel: "true"}},
			SPIFFEIDTemplate:  "spiffe://{{ .TrustDomain }}/ns/{{ .NamespaceMeta.Name }}/sa/{{ .ServiceAccountMeta.Name }}",
			ParentIDTemplate:  "spiffe://{{ .TrustDomain }}/ztunnel",
		},
	}
}

func serviceAccountNamespace(name string, enrolled bool) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{enrollmentLabel: fmt.Sprint(enrolled)}}}
}

func serviceAccount(namespace, name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

func newServiceAccountTestReconciler(t *testing.T, objects ...client.Object) (*entryReconciler, *serviceAccountTestClient, *serviceAccountEntryClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, spirev1alpha1.AddToScheme(scheme))
	k8s := &serviceAccountTestClient{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
			WithStatusSubresource(&spirev1alpha1.ClusterServiceAccountSPIFFEID{}, &spirev1alpha1.ClusterStaticEntry{}, &spirev1alpha1.ClusterSPIFFEID{}).Build(),
		t: t, lists: map[string]int{}, gets: map[string]int{}, listFailures: map[string]error{},
	}
	store := &serviceAccountEntryClient{entries: map[string]spireapi.Entry{}}
	return &entryReconciler{
		config: ReconcilerConfig{
			TrustDomain: spiffeid.RequireTrustDomainFromString(trustDomain), ClusterName: clusterName,
			K8sClient: k8s, EntryClient: store, EntryIDPrefix: "poc.",
			Reconcile: spirev1alpha1.ReconcileConfig{ClusterServiceAccountSPIFFEIDs: true},
		},
		promCounter: metrics.PromCounters,
	}, k8s, store
}

func serviceAccountStatus(t *testing.T, c *serviceAccountTestClient, name string) spirev1alpha1.ClusterServiceAccountSPIFFEIDStatus {
	t.Helper()
	var obj spirev1alpha1.ClusterServiceAccountSPIFFEID
	require.NoError(t, c.Client.Get(context.Background(), types.NamespacedName{Name: name}, &obj))
	return obj.Status
}

func updateTestObject(t *testing.T, c *serviceAccountTestClient, object client.Object, change func()) {
	t.Helper()
	require.NoError(t, c.Client.Get(context.Background(), client.ObjectKeyFromObject(object), object))
	change()
	require.NoError(t, c.Update(context.Background(), object))
}

type serviceAccountTestClient struct {
	client.Client
	t            *testing.T
	lists, gets  map[string]int
	listFailures map[string]error
	allowPodAPIs bool
}

func (c *serviceAccountTestClient) List(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	key := fmt.Sprintf("%T", obj)
	c.lists[key]++
	if err := c.listFailures[key]; err != nil {
		return err
	}
	switch obj.(type) {
	case *corev1.PodList, *corev1.NodeList, *corev1.EndpointsList:
		if !c.allowPodAPIs {
			c.t.Errorf("unexpected list of %T on service-account path", obj)
			return fmt.Errorf("forbidden list of %T", obj)
		}
	}
	return c.Client.List(ctx, obj, opts...)
}

func (c *serviceAccountTestClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets[fmt.Sprintf("%T", obj)]++
	switch obj.(type) {
	case *corev1.Pod, *corev1.Node, *corev1.Endpoints:
		if !c.allowPodAPIs {
			c.t.Errorf("unexpected get of %T on service-account path", obj)
			return fmt.Errorf("forbidden get of %T", obj)
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type serviceAccountEntryClient struct {
	entries                                  map[string]spireapi.Entry
	listErr, createErr, updateErr, deleteErr error
	createStatus, deleteStatus               codes.Code
	malformedCreate, lostCreateResponse      bool
	createCalls                              int
}

func (c *serviceAccountEntryClient) GetUnsupportedFields(context.Context, string) (map[spireapi.Field]struct{}, error) {
	return nil, nil
}

func (c *serviceAccountEntryClient) ListEntries(context.Context) ([]spireapi.Entry, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	var entries []spireapi.Entry
	for _, entry := range c.entries {
		entries = append(entries, entry)
	}
	return entries, nil
}

func (c *serviceAccountEntryClient) CreateEntries(_ context.Context, entries []spireapi.Entry) ([]spireapi.Status, error) {
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

func (c *serviceAccountEntryClient) UpdateEntries(_ context.Context, entries []spireapi.Entry) ([]spireapi.Status, error) {
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	statuses := make([]spireapi.Status, len(entries))
	for _, entry := range entries {
		c.entries[entry.ID] = entry
	}
	return statuses, nil
}

func (c *serviceAccountEntryClient) DeleteEntries(_ context.Context, ids []string) ([]spireapi.Status, error) {
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

func (c *serviceAccountEntryClient) ids() []string {
	ids := make([]string, 0, len(c.entries))
	for id := range c.entries {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (c *serviceAccountEntryClient) hasSPIFFEID(id string) bool {
	for _, entry := range c.entries {
		if entry.SPIFFEID.String() == id {
			return true
		}
	}
	return false
}
