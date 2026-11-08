package spireentry

import (
	"context"
	"slices"
	"testing"
	"time"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/pkg/metrics"
	"github.com/spiffe/spire-controller-manager/pkg/spireapi"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestServiceAccountSelectorTemplatesRendering(t *testing.T) {
	for _, tt := range []struct {
		name      string
		templates []string
		want      []spireapi.Selector
		err       string
	}{
		{name: "default", want: []spireapi.Selector{{Type: "k8s", Value: "ns:team"}, {Type: "k8s", Value: "sa:app"}}},
		{name: "equivalent sorted set", templates: []string{
			"k8s:sa:{{ .ServiceAccountMeta.Name }}", "k8s:ns:{{ .NamespaceMeta.Name }}", "k8s:sa:{{ .ServiceAccountMeta.Name }}",
		}, want: []spireapi.Selector{{Type: "k8s", Value: "ns:team"}, {Type: "k8s", Value: "sa:app"}}},
		{name: "only explicit selector", templates: []string{"k8s:pod-label:app:{{ .ServiceAccountMeta.Labels.app }}"}, want: []spireapi.Selector{{Type: "k8s", Value: "pod-label:app:web"}}},
		{name: "arbitrary types and metadata", templates: []string{
			`{{ .NamespaceMeta.Annotations.plugin }}:{{ index .ServiceAccountMeta.Annotations "example.org/resource" }}`,
			`other:{{ .TrustDomain }}:{{ .ClusterName }}:{{ .NamespaceMeta.Labels.team }}`,
		}, want: []spireapi.Selector{{Type: "custom", Value: "  value:with:colons, and spaces  "}, {Type: "other", Value: "example.org:test:payments"}}},
		{name: "namespace only", templates: []string{"k8s:ns:{{ .NamespaceMeta.Name }}"}, want: []spireapi.Selector{{Type: "k8s", Value: "ns:team"}}},
		{name: "no PodMeta", templates: []string{"k8s:ns:{{ .PodMeta.Namespace }}"}, err: "can't evaluate field PodMeta"},
		{name: "no PodSpec", templates: []string{"k8s:sa:{{ .PodSpec.ServiceAccountName }}"}, err: "can't evaluate field PodSpec"},
		{name: "no NodeMeta", templates: []string{"custom:{{ .NodeMeta.Name }}"}, err: "can't evaluate field NodeMeta"},
		{name: "missing label", templates: []string{"custom:{{ .ServiceAccountMeta.Labels.missing }}"}, err: "map has no entry"},
		{name: "empty rendering", templates: []string{`{{ if false }}custom:value{{ end }}`}, err: "invalid workload selector"},
		{name: "missing colon", templates: []string{"invalid"}, err: "invalid workload selector"},
		{name: "empty type", templates: []string{":value"}, err: "type cannot be empty"},
		{name: "empty value", templates: []string{"custom:"}, err: "value cannot be empty"},
		{name: "blank type", templates: []string{" \t:value"}, err: "type and value must not be blank"},
		{name: "blank value", templates: []string{"custom: \t"}, err: "type and value must not be blank"},
		{name: "missing indexed annotation yields empty value", templates: []string{`custom:{{ index .ServiceAccountMeta.Annotations "missing" }}`}, err: "value cannot be empty"},
		{name: "no partial selector list", templates: []string{"k8s:ns:team", "invalid"}, err: "workloadSelectorTemplates[1]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rule := serviceAccountRule("identities")
			if tt.templates != nil {
				rule.Spec.WorkloadSelectorTemplates = &tt.templates
			}
			spec, err := spirev1alpha1.ParseClusterServiceAccountSPIFFEIDSpec(&rule.Spec)
			require.NoError(t, err)
			ns := serviceAccountNamespace("team", true)
			ns.Labels["team"] = "payments"
			ns.Annotations = map[string]string{"plugin": "custom"}
			sa := serviceAccount("team", "app")
			sa.Labels = map[string]string{"app": "web"}
			sa.Annotations = map[string]string{"example.org/resource": "  value:with:colons, and spaces  "}
			r, _, _ := newServiceAccountTestReconciler(t)
			entry, err := renderServiceAccountEntry(spec, ns, sa, r.config.TrustDomain, r.config.ClusterName)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				require.Nil(t, entry)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, entry.Selectors)
		})
	}
}

func TestServiceAccountSelectorTemplatesPreserveBaselineIDs(t *testing.T) {
	ctx := context.Background()
	rule := serviceAccountRule("identities")
	ns := serviceAccountNamespace("team", true)
	r, k8s, store := newServiceAccountTestReconciler(t, rule, ns, serviceAccount("team", "app"))
	r.reconcile(ctx)
	// This ID is the baseline 1334fabe3976 algorithm's independent test vector.
	const baselineID = "poc.serviceaccount.0002496f-ec7a-54e4-a2c7-ae947cc8b70e.0a4f39d4-e1ee-50ab-a4da-fcda7c65dd41"
	require.Equal(t, []string{baselineID}, store.ids())
	baselineEntry := store.entries[baselineID]
	for _, templates := range []*[]string{
		{"k8s:ns:{{ .NamespaceMeta.Name }}", "k8s:sa:{{ .ServiceAccountMeta.Name }}"},
		{"k8s:sa:{{ .ServiceAccountMeta.Name }}", "k8s:ns:{{ .NamespaceMeta.Name }}", "k8s:ns:{{ .NamespaceMeta.Name }}"},
		nil,
	} {
		updateTestObject(t, k8s, rule, func() {
			rule.Generation++
			rule.Spec.WorkloadSelectorTemplates = templates
		})
		r = &entryReconciler{config: r.config, promCounter: metrics.PromCounters}
		r.reconcile(ctx)
		require.Equal(t, map[string]spireapi.Entry{baselineID: baselineEntry}, store.entries)
		require.Equal(t, 1, store.createCalls)
		require.Zero(t, store.updateCalls)
		require.Zero(t, store.deleteCalls)
	}
	updateTestObject(t, k8s, ns, func() { ns.Labels[enrollmentLabel] = "false" })
	r.reconcile(ctx)
	require.Empty(t, store.entries)
	updateTestObject(t, k8s, ns, func() { ns.Labels[enrollmentLabel] = "true" })
	r.reconcile(ctx)
	require.Equal(t, []string{baselineID}, store.ids())
}

func TestServiceAccountSelectorTemplatesCoalesceCandidates(t *testing.T) {
	for _, tt := range []struct {
		name, identity, selector string
		want                     int
	}{
		{"namespace identity", "spiffe://{{ .TrustDomain }}/ns/{{ .NamespaceMeta.Name }}", "k8s:ns:{{ .NamespaceMeta.Name }}", 2},
		{"cross namespace identity", "spiffe://{{ .TrustDomain }}/shared", "unix:uid:1000", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			rule := serviceAccountRule("identities")
			rule.Spec.SPIFFEIDTemplate = tt.identity
			rule.Spec.WorkloadSelectorTemplates = &[]string{tt.selector, tt.selector}
			r, k8s, store := newServiceAccountTestReconciler(t, rule,
				serviceAccountNamespace("team", true), serviceAccount("team", "app"), serviceAccount("team", "idle"),
				serviceAccountNamespace("second", true), serviceAccount("second", "app"))
			r.reconcile(ctx)
			require.Len(t, store.entries, tt.want)
			status := serviceAccountStatus(t, k8s, rule.Name)
			require.Equal(t, 3, status.Stats.ServiceAccountsSelected)
			require.Equal(t, tt.want, status.Stats.EntriesToSet)
			require.Zero(t, status.Stats.EntriesMasked)
			for _, entry := range store.entries {
				require.Len(t, entry.Selectors, 1)
			}
			originalIDs := store.ids()
			r.config.K8sClient = reverseServiceAccountLists{Client: k8s}
			r.reconcile(ctx)
			require.Equal(t, originalIDs, store.ids())
			require.Equal(t, 1, store.createCalls)
			require.Zero(t, store.updateCalls)
			require.Zero(t, store.deleteCalls)

			other := rule.DeepCopy()
			other.ObjectMeta = metav1.ObjectMeta{Name: "other", UID: "other", CreationTimestamp: metav1.NewTime(time.Unix(2, 0))}
			require.NoError(t, k8s.Create(ctx, other))
			r.reconcile(ctx)
			require.Equal(t, tt.want, serviceAccountStatus(t, k8s, other.Name).Stats.EntriesMasked)
			require.NoError(t, k8s.Delete(ctx, rule))
			r.reconcile(ctx)
			require.Equal(t, originalIDs, store.ids())
			require.Equal(t, tt.want, serviceAccountStatus(t, k8s, other.Name).Stats.EntriesToSet)

			require.NoError(t, k8s.Delete(ctx, serviceAccount("team", "idle")))
			r.reconcile(ctx)
			require.Equal(t, originalIDs, store.ids(), "entry remains while another candidate declares it")
			require.NoError(t, k8s.Delete(ctx, serviceAccount("team", "app")))
			r.reconcile(ctx)
			require.Len(t, store.entries, 1)
			require.NoError(t, k8s.Delete(ctx, serviceAccount("second", "app")))
			r.reconcile(ctx)
			require.Empty(t, store.entries, "last candidate removal collects the shared entry")
			require.Zero(t, k8s.lists["*v1.PodList"])
			require.Zero(t, k8s.lists["*v1.NodeList"])
			require.Zero(t, k8s.lists["*v1.EndpointsList"])
		})
	}
}

func TestServiceAccountSelectorTemplatesMetadataLifecycle(t *testing.T) {
	ctx := context.Background()
	rule := serviceAccountRule("identities")
	rule.Spec.WorkloadSelectorTemplates = &[]string{
		"k8s:ns:{{ .NamespaceMeta.Name }}",
		"k8s:sa:{{ .ServiceAccountMeta.Name }}",
		"k8s:pod-label:app:{{ .ServiceAccountMeta.Labels.app }}",
		"custom:tenant:{{ .NamespaceMeta.Annotations.tenant }}",
	}
	ns := serviceAccountNamespace("team", true)
	ns.Annotations = map[string]string{"tenant": "payments:east"}
	sa := serviceAccount("team", "app")
	sa.Labels = map[string]string{"app": "frontend"}
	r, k8s, store := newServiceAccountTestReconciler(t, rule, ns, sa)
	check := func(app, tenant string) string {
		t.Helper()
		r.reconcile(ctx)
		require.Len(t, store.entries, 1)
		id := store.ids()[0]
		require.Equal(t, []spireapi.Selector{
			{Type: "custom", Value: "tenant:" + tenant},
			{Type: "k8s", Value: "ns:team"},
			{Type: "k8s", Value: "pod-label:app:" + app},
			{Type: "k8s", Value: "sa:app"},
		}, store.entries[id].Selectors)
		return id
	}
	initial := check("frontend", "payments:east")
	updateTestObject(t, k8s, sa, func() { sa.Labels["app"] = "backend" })
	changed := check("backend", "payments:east")
	require.NotEqual(t, initial, changed)
	updateTestObject(t, k8s, ns, func() { ns.Annotations["tenant"] = "payments:west" })
	require.NotEqual(t, changed, check("backend", "payments:west"))
	require.NoError(t, k8s.Delete(ctx, rule))
	r.reconcile(ctx)
	require.Empty(t, store.entries)
}

func TestServiceAccountSelectorTemplatesFailClosed(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		templates    []string
	}{
		{"empty list", "InvalidSpec", []string{}},
		{"blank template", "InvalidSpec", []string{" "}},
		{"invalid template", "InvalidSpec", []string{"{{"}},
		{"empty rendering", "RenderFailed", []string{"k8s:ns:team", `{{ if false }}custom:value{{ end }}`}},
		{"empty value", "RenderFailed", []string{"k8s:ns:team", "custom:"}},
		{"unsupported Pod context", "RenderFailed", []string{"k8s:ns:team", "k8s:sa:{{ .PodSpec.ServiceAccountName }}"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			rule := serviceAccountRule("identities")
			r, k8s, store := newServiceAccountTestReconciler(t, rule, serviceAccountNamespace("team", true), serviceAccount("team", "app"))
			r.reconcile(ctx)
			require.Len(t, store.entries, 1)
			updateTestObject(t, k8s, rule, func() {
				rule.Generation++
				rule.Spec.WorkloadSelectorTemplates = &tt.templates
			})
			r.reconcile(ctx)
			require.Empty(t, store.entries, "invalid explicit selectors must not retain or fall back to default selectors")
			status := serviceAccountStatus(t, k8s, rule.Name)
			condition := meta.FindStatusCondition(status.Conditions, "Ready")
			require.Equal(t, metav1.ConditionFalse, condition.Status)
			require.Equal(t, tt.reason, condition.Reason)
			require.Contains(t, condition.Message, "workloadSelectorTemplates")
			require.Equal(t, rule.Generation, status.ObservedGeneration)
			r.reconcile(ctx)
			require.Equal(t, status, serviceAccountStatus(t, k8s, rule.Name))
			updateTestObject(t, k8s, rule, func() {
				rule.Generation++
				rule.Spec.WorkloadSelectorTemplates = &[]string{"custom:recovered"}
			})
			r.reconcile(ctx)
			require.Len(t, store.entries, 1)
			for _, entry := range store.entries {
				require.Equal(t, []spireapi.Selector{{Type: "custom", Value: "recovered"}}, entry.Selectors)
			}
			status = serviceAccountStatus(t, k8s, rule.Name)
			require.Equal(t, metav1.ConditionTrue, meta.FindStatusCondition(status.Conditions, "Ready").Status)
		})
	}
}

func TestServiceAccountSelectorTemplatesCannotConflateTuples(t *testing.T) {
	ctx := context.Background()
	rule := serviceAccountRule("identities")
	rule.Spec.SPIFFEIDTemplate = "spiffe://{{ .TrustDomain }}/shared"
	rule.Spec.WorkloadSelectorTemplates = &[]string{"{{ .ServiceAccountMeta.Annotations.selector }}"}
	a, b := serviceAccount("team", "a"), serviceAccount("team", "b")
	a.Annotations = map[string]string{"selector": "a:bc"}
	b.Annotations = map[string]string{"selector": "ab:c"}
	r, _, store := newServiceAccountTestReconciler(t, rule, serviceAccountNamespace("team", true), a, b)
	r.reconcile(ctx)
	require.Len(t, store.entries, 2, "arbitrary type/value boundaries must produce distinct keys and IDs")
	for id, entry := range store.entries {
		require.Equal(t, r.serviceAccountEntryID(entry), id)
	}
	ids := store.ids()
	r.reconcile(ctx)
	require.Equal(t, ids, store.ids())
	require.Equal(t, 1, store.createCalls)
}

type reverseServiceAccountLists struct{ client.Client }

func (c reverseServiceAccountLists) List(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, obj, opts...); err != nil {
		return err
	}
	switch list := obj.(type) {
	case *corev1.NamespaceList:
		slices.Reverse(list.Items)
	case *corev1.ServiceAccountList:
		slices.Reverse(list.Items)
	}
	return nil
}
