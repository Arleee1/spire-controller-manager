package v1alpha1_test

import (
	"testing"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseClusterSPIFFEIDSet(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*spirev1alpha1.ClusterSPIFFEIDSetSpec)
		err    string
	}{
		{name: "constant"},
		{name: "empty inputs", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) { s.ForEach = []spirev1alpha1.SPIFFEIDSetInput{} }},
		{name: "custom kind", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) {
			s.ForEach = []spirev1alpha1.SPIFFEIDSetInput{{SPIFFEIDSetKind: spirev1alpha1.SPIFFEIDSetKind{APIVersion: "example.org/v1", Kind: "Workload"}, Alias: "workload"}}
		}},
		{name: "missing selector list", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) { s.WorkloadSelectorTemplates = nil }, err: "at least one"},
		{name: "empty selector list", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) { s.WorkloadSelectorTemplates = []string{} }, err: "at least one"},
		{name: "blank selector template", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) { s.WorkloadSelectorTemplates = []string{" \t"} }, err: "workloadSelectorTemplates[0]"},
		{name: "invalid selector template", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) { s.WorkloadSelectorTemplates = []string{"{{"} }, err: "workloadSelectorTemplates[0]"},
		{name: "empty identity", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) { s.SPIFFEIDTemplate = "" }, err: "spiffeIDTemplate"},
		{name: "empty parent", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) { s.ParentIDTemplate = "" }, err: "parentIDTemplate"},
		{name: "duplicate aliases", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) {
			input := spirev1alpha1.SPIFFEIDSetInput{SPIFFEIDSetKind: spirev1alpha1.SPIFFEIDSetKind{APIVersion: "v1", Kind: "Namespace"}, Alias: "ns"}
			s.ForEach = []spirev1alpha1.SPIFFEIDSetInput{input, input}
		}, err: "duplicate alias"},
		{name: "invalid alias", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) {
			s.ForEach = []spirev1alpha1.SPIFFEIDSetInput{{Alias: "invalid-name"}}
		}, err: "invalid alias"},
		{name: "invalid label selector", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) {
			s.ForEach = []spirev1alpha1.SPIFFEIDSetInput{{
				SPIFFEIDSetKind: spirev1alpha1.SPIFFEIDSetKind{APIVersion: "v1", Kind: "Namespace"}, Alias: "ns",
				LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: "Invalid"}}},
			}}
		}, err: "labelSelector"},
		{name: "empty namespace template", change: func(s *spirev1alpha1.ClusterSPIFFEIDSetSpec) {
			value := ""
			s.ForEach = []spirev1alpha1.SPIFFEIDSetInput{{SPIFFEIDSetKind: spirev1alpha1.SPIFFEIDSetKind{APIVersion: "v1", Kind: "ServiceAccount"}, Alias: "sa", NamespaceTemplate: &value}}
		}, err: "namespaceTemplate"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := &spirev1alpha1.ClusterSPIFFEIDSetSpec{SPIFFEIDTemplate: "spiffe://{{ .TrustDomain }}/constant", ParentIDTemplate: "spiffe://{{ .TrustDomain }}/parent", WorkloadSelectorTemplates: []string{"custom:constant"}}
			if tt.change != nil {
				tt.change(spec)
			}
			parsed, err := spirev1alpha1.ParseClusterSPIFFEIDSetSpec(spec)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.Len(t, parsed.ForEach, len(spec.ForEach))
		})
	}
}

func TestSPIFFEIDSetKindPolicy(t *testing.T) {
	for _, tt := range []struct{ apiVersion, kind, err string }{
		{"v1", "Namespace", ""},
		{"v1", "ServiceAccount", ""},
		{"example.org/v1alpha1", "Workload", ""},
		{"example.org/v1", "Secret", ""},
		{"v1", "Pod", "forbidden"},
		{"v1", "Secret", "forbidden"},
		{"v1", "PodList", "not a list"},
		{"*", "ConfigMap", "invalid input kind"},
		{"v1", "*", "invalid input kind"},
		{"v1", "pods/log", "invalid input kind"},
		{"bad group/v1", "Workload", "invalid input kind"},
		{"", "Namespace", "invalid input kind"},
	} {
		t.Run(tt.apiVersion+"-"+tt.kind, func(t *testing.T) {
			_, err := spirev1alpha1.ParseSPIFFEIDSetKind(spirev1alpha1.SPIFFEIDSetKind{APIVersion: tt.apiVersion, Kind: tt.kind})
			if tt.err == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.err)
			}
		})
	}
}

func TestSPIFFEIDSetDeepCopy(t *testing.T) {
	ns := "{{ .Inputs.ns.metadata.name }}"
	original := &spirev1alpha1.ClusterSPIFFEIDSet{Spec: spirev1alpha1.ClusterSPIFFEIDSetSpec{
		ForEach:                   []spirev1alpha1.SPIFFEIDSetInput{{NamespaceTemplate: &ns, LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "yes"}}}},
		WorkloadSelectorTemplates: []string{"custom:original"},
	}}
	copied := original.DeepCopy()
	*copied.Spec.ForEach[0].NamespaceTemplate = "changed"
	copied.Spec.ForEach[0].LabelSelector.MatchLabels["app"] = "no"
	copied.Spec.WorkloadSelectorTemplates[0] = "custom:changed"
	require.Equal(t, ns, *original.Spec.ForEach[0].NamespaceTemplate)
	require.Equal(t, "yes", original.Spec.ForEach[0].LabelSelector.MatchLabels["app"])
	require.Equal(t, "custom:original", original.Spec.WorkloadSelectorTemplates[0])
}
