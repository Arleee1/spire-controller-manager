package v1alpha1

import (
	"fmt"
	"strings"
	"text/template"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// +kubebuilder:object:generate=false

// ParsedClusterServiceAccountSPIFFEIDSpec is a validated service-account identity rule.
type ParsedClusterServiceAccountSPIFFEIDSpec struct {
	NamespaceSelector         labels.Selector
	ServiceAccountSelector    labels.Selector
	SPIFFEIDTemplate          *template.Template
	ParentIDTemplate          *template.Template
	WorkloadSelectorTemplates []*template.Template
}

// ParseClusterServiceAccountSPIFFEIDSpec parses selectors and strict Go templates.
func ParseClusterServiceAccountSPIFFEIDSpec(spec *ClusterServiceAccountSPIFFEIDSpec) (*ParsedClusterServiceAccountSPIFFEIDSpec, error) {
	namespaceSelector, err := metav1.LabelSelectorAsSelector(&spec.NamespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid namespaceSelector: %w", err)
	}
	serviceAccountSelector := labels.Everything()
	if spec.ServiceAccountSelector != nil {
		serviceAccountSelector, err = metav1.LabelSelectorAsSelector(spec.ServiceAccountSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid serviceAccountSelector: %w", err)
		}
	}
	spiffeIDTemplate, err := parseServiceAccountTemplate("spiffeIDTemplate", spec.SPIFFEIDTemplate)
	if err != nil {
		return nil, err
	}
	parentIDTemplate, err := parseServiceAccountTemplate("parentIDTemplate", spec.ParentIDTemplate)
	if err != nil {
		return nil, err
	}
	var workloadSelectorTemplates []*template.Template
	if spec.WorkloadSelectorTemplates != nil {
		if len(*spec.WorkloadSelectorTemplates) == 0 {
			return nil, fmt.Errorf("workloadSelectorTemplates must contain at least one template when specified")
		}
		for i, value := range *spec.WorkloadSelectorTemplates {
			tmpl, err := parseServiceAccountTemplate(fmt.Sprintf("workloadSelectorTemplates[%d]", i), value)
			if err != nil {
				return nil, err
			}
			workloadSelectorTemplates = append(workloadSelectorTemplates, tmpl)
		}
	}
	return &ParsedClusterServiceAccountSPIFFEIDSpec{
		NamespaceSelector:         namespaceSelector,
		ServiceAccountSelector:    serviceAccountSelector,
		SPIFFEIDTemplate:          spiffeIDTemplate,
		ParentIDTemplate:          parentIDTemplate,
		WorkloadSelectorTemplates: workloadSelectorTemplates,
	}, nil
}

func parseServiceAccountTemplate(name, value string) (*template.Template, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("%s must not be empty", name)
	}
	tmpl, err := template.New(name).Option("missingkey=error").Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", name, err)
	}
	return tmpl, nil
}
