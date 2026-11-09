package spireentry

import (
	"context"
	"testing"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/spiffe/spire-controller-manager/pkg/spireapi"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIdentitySetSelectorTemplates(t *testing.T) {
	for _, tt := range []struct {
		name      string
		templates []string
		want      []spireapi.Selector
		err       string
	}{
		{name: "canonical set", templates: []string{"k8s:sa:app", "k8s:ns:team", "k8s:sa:app"}, want: []spireapi.Selector{{Type: "k8s", Value: "ns:team"}, {Type: "k8s", Value: "sa:app"}}},
		{name: "explicit only", templates: []string{"custom:{{ .Inputs.sa.metadata.labels.app }}"}, want: []spireapi.Selector{{Type: "custom", Value: "web"}}},
		{name: "payload preserved", templates: []string{`{{ index .Inputs.sa.metadata.annotations "example.org/selector" }}`}, want: []spireapi.Selector{{Type: "custom", Value: "  value:with:colons, and spaces  "}}},
		{name: "no PodMeta", templates: []string{"k8s:ns:{{ .PodMeta.Name }}"}, err: "can't evaluate field PodMeta"},
		{name: "no synthetic SA context", templates: []string{"k8s:sa:{{ .ServiceAccountMeta.Name }}"}, err: "can't evaluate field ServiceAccountMeta"},
		{name: "missing label", templates: []string{"custom:{{ .Inputs.sa.metadata.labels.missing }}"}, err: "map has no entry"},
		{name: "empty rendering", templates: []string{`{{ if false }}custom:value{{ end }}`}, err: "invalid workload selector"},
		{name: "missing colon", templates: []string{"invalid"}, err: "invalid workload selector"},
		{name: "empty type", templates: []string{":value"}, err: "type cannot be empty"},
		{name: "empty value", templates: []string{"custom:"}, err: "value cannot be empty"},
		{name: "blank type", templates: []string{" \t:value"}, err: "type and value must not be blank"},
		{name: "blank value", templates: []string{"custom: \t"}, err: "type and value must not be blank"},
		{name: "no partial constraints", templates: []string{"k8s:ns:team", "invalid"}, err: "workloadSelectorTemplates[1]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			rule := constantSet("set")
			rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("sa", "ServiceAccount", "")}
			sa := setServiceAccount("team", "app")
			sa.Labels = map[string]string{"app": "web"}
			sa.Annotations = map[string]string{"example.org/selector": "custom:  value:with:colons, and spaces  "}
			r, k8s, store, _ := newIdentitySetTest(t, rule, sa)
			r.reconcile(ctx)
			require.Len(t, store.entries, 1)
			updateSetObject(t, k8s, rule, func() { rule.Spec.WorkloadSelectorTemplates = tt.templates })
			r.reconcile(ctx)
			condition := meta.FindStatusCondition(setStatus(t, k8s, rule.Name).Conditions, "Ready")
			if tt.err != "" {
				require.Equal(t, metav1.ConditionFalse, condition.Status)
				require.Contains(t, condition.Message, tt.err)
				require.Empty(t, store.entries, "invalid constraints must not retain a weaker registration")
			} else {
				require.Equal(t, metav1.ConditionTrue, condition.Status)
				require.Len(t, store.entries, 1)
				for _, entry := range store.entries {
					require.Equal(t, tt.want, entry.Selectors)
				}
			}
		})
	}
}

func TestIdentitySetSelectorBoundaries(t *testing.T) {
	rule := constantSet("set")
	rule.Spec.ForEach = []spirev1alpha1.SPIFFEIDSetInput{setInput("sa", "ServiceAccount", "")}
	rule.Spec.WorkloadSelectorTemplates = []string{"{{ .Inputs.sa.metadata.annotations.selector }}"}
	a, b := setServiceAccount("team", "a"), setServiceAccount("team", "b")
	a.Annotations, b.Annotations = map[string]string{"selector": "a:bc"}, map[string]string{"selector": "ab:c"}
	r, _, store, _ := newIdentitySetTest(t, rule, a, b)
	r.reconcile(context.Background())
	require.Len(t, store.entries, 2, "arbitrary selector type/value boundaries must produce distinct keys and IDs")
	for id, entry := range store.entries {
		require.Equal(t, deterministicEntryID(r.identitySetEntryIDPrefix(), entry), id)
	}
	ids := store.ids()
	r.reconcile(context.Background())
	require.Equal(t, ids, store.ids())
	require.Equal(t, 1, store.createCalls)
}

func TestIdentitySetScopeFraming(t *testing.T) {
	first := scopedEntryIDPrefix("prefix."+identitySetEntryIDMarker, "example.org", "a", "bc")
	second := scopedEntryIDPrefix("prefix."+identitySetEntryIDMarker, "example.org", "ab", "c")
	require.NotEqual(t, first, second)
	require.NotEqual(t, first, scopedEntryIDPrefix("other."+identitySetEntryIDMarker, "example.org", "a", "bc"))
	require.Equal(t, first, scopedEntryIDPrefix("prefix."+identitySetEntryIDMarker, "example.org", "a", "bc"))
}
