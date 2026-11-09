package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SPIFFEIDSetKind identifies one explicitly allowed Kubernetes input kind.
type SPIFFEIDSetKind struct {
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9]*$`
	Kind string `json:"kind"`
}

// SPIFFEIDSetInput adds an object binding to each combination of earlier inputs.
type SPIFFEIDSetInput struct {
	SPIFFEIDSetKind `json:",inline"`

	// Alias is the unique key under Inputs available to subsequent templates.
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9_]*$`
	Alias string `json:"alias"`

	// LabelSelector filters objects of this input kind.
	// +optional
	LabelSelector *metav1.LabelSelector `json:"labelSelector,omitempty"`

	// NamespaceTemplate restricts a namespaced input using earlier bindings.
	// Omission selects all namespaces without reading Namespace objects.
	// +optional
	// +kubebuilder:validation:MinLength=1
	NamespaceTemplate *string `json:"namespaceTemplate,omitempty"`
}

// ClusterSPIFFEIDSetSpec declares identities over explicit Kubernetes inputs.
type ClusterSPIFFEIDSetSpec struct {
	// +optional
	ClassName string `json:"className,omitempty"`

	// ForEach is evaluated in order. Omission or an empty list yields one constant candidate.
	// +optional
	// +listType=atomic
	ForEach []SPIFFEIDSetInput `json:"forEach,omitempty"`

	// SPIFFEIDTemplate uses TrustDomain, ClusterName and Inputs.
	// +kubebuilder:validation:MinLength=1
	SPIFFEIDTemplate string `json:"spiffeIDTemplate"`

	// ParentIDTemplate uses the same context as SPIFFEIDTemplate.
	// +kubebuilder:validation:MinLength=1
	ParentIDTemplate string `json:"parentIDTemplate"`

	// WorkloadSelectorTemplates is the complete selector set; nothing is implicitly appended.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +listType=atomic
	WorkloadSelectorTemplates []string `json:"workloadSelectorTemplates"`
}

// ClusterSPIFFEIDSetStats describes the last shared entry reconciliation.
type ClusterSPIFFEIDSetStats struct {
	Combinations        int `json:"combinations"`
	EntryRenderFailures int `json:"entryRenderFailures"`
	EntriesMasked       int `json:"entriesMasked"`
	EntriesToSet        int `json:"entriesToSet"`
	EntryFailures       int `json:"entryFailures"`
}

// ClusterSPIFFEIDSetStatus reports input, template and SPIRE failures explicitly.
type ClusterSPIFFEIDSetStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	Stats ClusterSPIFFEIDSetStats `json:"stats,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Entries",type=integer,JSONPath=`.status.stats.entriesToSet`

// ClusterSPIFFEIDSet declares a set of identities over explicitly selected inputs.
type ClusterSPIFFEIDSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterSPIFFEIDSetSpec `json:"spec"`
	// +optional
	Status ClusterSPIFFEIDSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterSPIFFEIDSetList contains ClusterSPIFFEIDSet resources.
type ClusterSPIFFEIDSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterSPIFFEIDSet `json:"items"`
}
