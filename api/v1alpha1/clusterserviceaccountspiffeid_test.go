package v1alpha1_test

import (
	"encoding/json"
	"testing"

	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func TestParseClusterServiceAccountSPIFFEIDSpec(t *testing.T) {
	valid := spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec{
		NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"io.cilium/mtls-enabled": "true"}},
		SPIFFEIDTemplate:  "spiffe://{{ .TrustDomain }}/ns/{{ .NamespaceMeta.Name }}/sa/{{ .ServiceAccountMeta.Name }}",
		ParentIDTemplate:  "spiffe://{{ .TrustDomain }}/ztunnel",
	}
	for _, tt := range []struct {
		name   string
		change func(*spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec)
		err    string
	}{
		{name: "all service accounts"},
		{name: "service account selector", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) {
			s.ServiceAccountSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "workload", Operator: metav1.LabelSelectorOpIn, Values: []string{"app"},
			}}}
		}},
		{name: "empty identity", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) { s.SPIFFEIDTemplate = "" }, err: "spiffeIDTemplate must not be empty"},
		{name: "blank parent", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) { s.ParentIDTemplate = " " }, err: "parentIDTemplate must not be empty"},
		{name: "identity syntax", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) { s.SPIFFEIDTemplate = "{{" }, err: "invalid spiffeIDTemplate"},
		{name: "parent syntax", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) { s.ParentIDTemplate = "{{" }, err: "invalid parentIDTemplate"},
		{name: "invalid namespace selector", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) {
			s.NamespaceSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "x", Operator: "Invalid"}}
		}, err: "invalid namespaceSelector"},
		{name: "invalid service account selector", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) {
			s.ServiceAccountSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: "Invalid"}}}
		}, err: "invalid serviceAccountSelector"},
		{name: "explicit selectors", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) {
			s.WorkloadSelectorTemplates = &[]string{"k8s:ns:{{ .NamespaceMeta.Name }}", "custom:{{ .ServiceAccountMeta.Name }}"}
		}},
		{name: "empty selector list", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) {
			s.WorkloadSelectorTemplates = &[]string{}
		}, err: "workloadSelectorTemplates must contain at least one template"},
		{name: "nil selector slice", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) {
			s.WorkloadSelectorTemplates = new([]string)
		}, err: "workloadSelectorTemplates must contain at least one template"},
		{name: "empty selector template", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) {
			s.WorkloadSelectorTemplates = &[]string{""}
		}, err: "workloadSelectorTemplates[0] must not be empty"},
		{name: "blank selector template", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) {
			s.WorkloadSelectorTemplates = &[]string{"k8s:ns:team", " \t"}
		}, err: "workloadSelectorTemplates[1] must not be empty"},
		{name: "selector template syntax", change: func(s *spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec) {
			s.WorkloadSelectorTemplates = &[]string{"{{"}
		}, err: "invalid workloadSelectorTemplates[0]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := valid.DeepCopy()
			if tt.change != nil {
				tt.change(spec)
			}
			parsed, err := spirev1alpha1.ParseClusterServiceAccountSPIFFEIDSpec(spec)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.True(t, parsed.NamespaceSelector.Matches(labels.Set{"io.cilium/mtls-enabled": "true"}))
			require.False(t, parsed.NamespaceSelector.Matches(labels.Set{}))
			require.True(t, parsed.ServiceAccountSelector.Matches(labels.Set{"workload": "app"}))
			require.Equal(t, spec.ServiceAccountSelector == nil, parsed.ServiceAccountSelector.Matches(labels.Set{}))
			if spec.WorkloadSelectorTemplates == nil {
				require.Nil(t, parsed.WorkloadSelectorTemplates)
			} else {
				require.Len(t, parsed.WorkloadSelectorTemplates, len(*spec.WorkloadSelectorTemplates))
			}
		})
	}
}

func TestClusterServiceAccountSPIFFEIDDeepCopy(t *testing.T) {
	original := &spirev1alpha1.ClusterServiceAccountSPIFFEID{
		Spec: spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec{
			NamespaceSelector:         metav1.LabelSelector{MatchLabels: map[string]string{"enrolled": "true"}},
			ServiceAccountSelector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "yes"}},
			WorkloadSelectorTemplates: &[]string{"k8s:ns:{{ .NamespaceMeta.Name }}"},
		},
		Status: spirev1alpha1.ClusterServiceAccountSPIFFEIDStatus{Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}},
	}
	copied := original.DeepCopy()
	copied.Spec.NamespaceSelector.MatchLabels["enrolled"] = "false"
	copied.Spec.ServiceAccountSelector.MatchLabels["app"] = "no"
	(*copied.Spec.WorkloadSelectorTemplates)[0] = "custom:changed"
	copied.Status.Conditions[0].Status = metav1.ConditionFalse
	require.Equal(t, "true", original.Spec.NamespaceSelector.MatchLabels["enrolled"])
	require.Equal(t, "yes", original.Spec.ServiceAccountSelector.MatchLabels["app"])
	require.Equal(t, []string{"k8s:ns:{{ .NamespaceMeta.Name }}"}, *original.Spec.WorkloadSelectorTemplates)
	require.Equal(t, metav1.ConditionTrue, original.Status.Conditions[0].Status)
}

func TestServiceAccountSelectorTemplatesJSON(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		present     bool
		size        int
	}{
		{name: "omitted", input: `{}`},
		{name: "null", input: `{"workloadSelectorTemplates":null}`},
		{name: "empty", input: `{"workloadSelectorTemplates":[]}`, present: true},
		{name: "explicit", input: `{"workloadSelectorTemplates":["custom:value:with:colons"]}`, present: true, size: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var spec spirev1alpha1.ClusterServiceAccountSPIFFEIDSpec
			require.NoError(t, json.Unmarshal([]byte(tt.input), &spec))
			copied := spec.DeepCopy()
			encoded, err := json.Marshal(copied)
			require.NoError(t, err)
			if !tt.present {
				require.Nil(t, copied.WorkloadSelectorTemplates)
				require.NotContains(t, string(encoded), "workloadSelectorTemplates")
				return
			}
			require.NotNil(t, copied.WorkloadSelectorTemplates)
			require.Len(t, *copied.WorkloadSelectorTemplates, tt.size)
			require.Contains(t, string(encoded), `"workloadSelectorTemplates":[`)
		})
	}
}
