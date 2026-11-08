package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ClusterServiceAccountSPIFFEIDSpec declares identities for selected namespace/service-account pairs.
type ClusterServiceAccountSPIFFEIDSpec struct {
	// ClassName selects the controller instance that handles this resource.
	// +optional
	ClassName string `json:"className,omitempty"`

	// NamespaceSelector selects enrolled namespaces. An empty selector selects all namespaces.
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector"`

	// ServiceAccountSelector selects service accounts within enrolled namespaces.
	// If omitted or empty, all service accounts are selected, including accounts with no Pods.
	// +optional
	ServiceAccountSelector *metav1.LabelSelector `json:"serviceAccountSelector,omitempty"`

	// SPIFFEIDTemplate is rendered with TrustDomain, ClusterName, NamespaceMeta and ServiceAccountMeta.
	// +kubebuilder:validation:MinLength=1
	SPIFFEIDTemplate string `json:"spiffeIDTemplate"`

	// ParentIDTemplate identifies the parent authorized to obtain these identities.
	// It has the same template context as SPIFFEIDTemplate; Pod and Node fields are not available.
	// +kubebuilder:validation:MinLength=1
	ParentIDTemplate string `json:"parentIDTemplate"`
}

// ClusterServiceAccountSPIFFEIDStatus reports the last shared entry reconciliation.
type ClusterServiceAccountSPIFFEIDStatus struct {
	// ObservedGeneration is the resource generation used for reconciliation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions report template, Kubernetes API and SPIRE API failures.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Stats contains counts from the last reconciliation.
	// +optional
	Stats ClusterServiceAccountSPIFFEIDStats `json:"stats,omitempty"`
}

// ClusterServiceAccountSPIFFEIDStats contains namespace/service-account reconciliation counts.
type ClusterServiceAccountSPIFFEIDStats struct {
	NamespacesSelected      int `json:"namespacesSelected"`
	NamespacesIgnored       int `json:"namespacesIgnored"`
	ServiceAccountsSelected int `json:"serviceAccountsSelected"`
	EntryRenderFailures     int `json:"entryRenderFailures"`
	EntriesMasked           int `json:"entriesMasked"`
	EntriesToSet            int `json:"entriesToSet"`
	EntryFailures           int `json:"entryFailures"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Service accounts",type=integer,JSONPath=`.status.stats.serviceAccountsSelected`
// +kubebuilder:printcolumn:name="Entries",type=integer,JSONPath=`.status.stats.entriesToSet`

// ClusterServiceAccountSPIFFEID is a cluster-scoped namespace/service-account identity rule.
type ClusterServiceAccountSPIFFEID struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ClusterServiceAccountSPIFFEIDSpec `json:"spec"`
	// +optional
	Status ClusterServiceAccountSPIFFEIDStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterServiceAccountSPIFFEIDList contains ClusterServiceAccountSPIFFEID resources.
type ClusterServiceAccountSPIFFEIDList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterServiceAccountSPIFFEID `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterServiceAccountSPIFFEID{}, &ClusterServiceAccountSPIFFEIDList{})
}
