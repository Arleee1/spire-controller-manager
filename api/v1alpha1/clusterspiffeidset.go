package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

var inputAliasPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
var inputKindPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

// +kubebuilder:object:generate=false

// ParsedSPIFFEIDSetInput contains the validated selection and namespace template.
type ParsedSPIFFEIDSetInput struct {
	Alias             string
	GVK               schema.GroupVersionKind
	LabelSelector     labels.Selector
	NamespaceTemplate *template.Template
}

// +kubebuilder:object:generate=false

// ParsedClusterSPIFFEIDSetSpec is a validated explicit-input identity declaration.
type ParsedClusterSPIFFEIDSetSpec struct {
	ForEach                   []ParsedSPIFFEIDSetInput
	SPIFFEIDTemplate          *template.Template
	ParentIDTemplate          *template.Template
	WorkloadSelectorTemplates []*template.Template
}

// ParseSPIFFEIDSetKind validates a concrete kind, excluding Pod and Secret inputs.
func ParseSPIFFEIDSetKind(kind SPIFFEIDSetKind) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(kind.APIVersion)
	if err != nil || gv.Version == "" || len(validation.IsDNS1035Label(gv.Version)) != 0 ||
		(gv.Group != "" && len(validation.IsDNS1123Subdomain(gv.Group)) != 0) || !inputKindPattern.MatchString(kind.Kind) {
		return schema.GroupVersionKind{}, fmt.Errorf("invalid input kind %q %q", kind.APIVersion, kind.Kind)
	}
	if gv.Group == "" && (kind.Kind == "Pod" || kind.Kind == "Secret") {
		return schema.GroupVersionKind{}, fmt.Errorf("input kind %s/%s is forbidden", kind.APIVersion, kind.Kind)
	}
	if strings.HasSuffix(kind.Kind, "List") {
		return schema.GroupVersionKind{}, fmt.Errorf("input kind must identify objects, not a list: %s", kind.Kind)
	}
	return gv.WithKind(kind.Kind), nil
}

// ParseClusterSPIFFEIDSetSpec parses ordered input bindings and strict templates.
func ParseClusterSPIFFEIDSetSpec(spec *ClusterSPIFFEIDSetSpec) (*ParsedClusterSPIFFEIDSetSpec, error) {
	out := new(ParsedClusterSPIFFEIDSetSpec)
	var err error
	out.SPIFFEIDTemplate, err = parseRequiredTemplate("spiffeIDTemplate", spec.SPIFFEIDTemplate)
	if err != nil {
		return nil, err
	}
	out.ParentIDTemplate, err = parseRequiredTemplate("parentIDTemplate", spec.ParentIDTemplate)
	if err != nil {
		return nil, err
	}
	if len(spec.WorkloadSelectorTemplates) == 0 {
		return nil, errors.New("workloadSelectorTemplates must contain at least one template")
	}
	for i, value := range spec.WorkloadSelectorTemplates {
		tmpl, err := parseRequiredTemplate(fmt.Sprintf("workloadSelectorTemplates[%d]", i), value)
		if err != nil {
			return nil, err
		}
		out.WorkloadSelectorTemplates = append(out.WorkloadSelectorTemplates, tmpl)
	}
	aliases := make(map[string]struct{})
	for i, input := range spec.ForEach {
		if !inputAliasPattern.MatchString(input.Alias) {
			return nil, fmt.Errorf("forEach[%d]: invalid alias %q", i, input.Alias)
		}
		if _, exists := aliases[input.Alias]; exists {
			return nil, fmt.Errorf("forEach[%d]: duplicate alias %q", i, input.Alias)
		}
		aliases[input.Alias] = struct{}{}
		gvk, err := ParseSPIFFEIDSetKind(input.SPIFFEIDSetKind)
		if err != nil {
			return nil, fmt.Errorf("forEach[%d]: %w", i, err)
		}
		parsed := ParsedSPIFFEIDSetInput{Alias: input.Alias, GVK: gvk, LabelSelector: labels.Everything()}
		if input.LabelSelector != nil {
			parsed.LabelSelector, err = metav1.LabelSelectorAsSelector(input.LabelSelector)
			if err != nil {
				return nil, fmt.Errorf("forEach[%d].labelSelector: %w", i, err)
			}
		}
		if input.NamespaceTemplate != nil {
			parsed.NamespaceTemplate, err = parseRequiredTemplate(fmt.Sprintf("forEach[%d].namespaceTemplate", i), *input.NamespaceTemplate)
			if err != nil {
				return nil, err
			}
		}
		out.ForEach = append(out.ForEach, parsed)
	}
	return out, nil
}

func parseRequiredTemplate(name, value string) (*template.Template, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("%s must not be empty", name)
	}
	tmpl, err := template.New(name).Option("missingkey=error").Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", name, err)
	}
	return tmpl, nil
}
