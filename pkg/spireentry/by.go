package spireentry

import (
	spirev1alpha1 "github.com/spiffe/spire-controller-manager/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type byObject interface {
	GetObjectKind() schema.ObjectKind

	GetUID() types.UID
	GetCreationTimestamp() metav1.Time
	GetDeletionTimestamp() *metav1.Time

	IncrementEntriesToSet()
	IncrementEntriesMasked()
	IncrementEntrySuccess()
	IncrementEntryFailures()
}

type ClusterStaticEntry struct {
	spirev1alpha1.ClusterStaticEntry
	NextStatus spirev1alpha1.ClusterStaticEntryStatus
}

func (by *ClusterStaticEntry) IncrementEntriesToSet() {
}

func (by *ClusterStaticEntry) IncrementEntriesMasked() {
	by.NextStatus.Masked = true
}

func (by *ClusterStaticEntry) IncrementEntrySuccess() {
	by.NextStatus.Set = true
}

func (by *ClusterStaticEntry) IncrementEntryFailures() {
}

type ClusterSPIFFEID struct {
	spirev1alpha1.ClusterSPIFFEID
	NextStatus spirev1alpha1.ClusterSPIFFEIDStatus
}

func (by *ClusterSPIFFEID) IncrementEntriesToSet() {
	by.NextStatus.Stats.EntriesToSet++
}

func (by *ClusterSPIFFEID) IncrementEntriesMasked() {
	by.NextStatus.Stats.EntriesMasked++
}

func (by *ClusterSPIFFEID) IncrementEntrySuccess() {
}

func (by *ClusterSPIFFEID) IncrementEntryFailures() {
	by.NextStatus.Stats.EntryFailures++
}

type ClusterServiceAccountSPIFFEID struct {
	spirev1alpha1.ClusterServiceAccountSPIFFEID
	NextStatus spirev1alpha1.ClusterServiceAccountSPIFFEIDStatus
}

func (by *ClusterServiceAccountSPIFFEID) IncrementEntriesToSet() {
	by.NextStatus.Stats.EntriesToSet++
}

func (by *ClusterServiceAccountSPIFFEID) IncrementEntriesMasked() {
	by.NextStatus.Stats.EntriesMasked++
}

func (by *ClusterServiceAccountSPIFFEID) IncrementEntrySuccess() {}

func (by *ClusterServiceAccountSPIFFEID) IncrementEntryFailures() {
	by.NextStatus.Stats.EntryFailures++
	by.setCondition(metav1.ConditionFalse, "SPIREAPIError", "Failed to create or update SPIRE entries; see controller logs")
}

func (by *ClusterServiceAccountSPIFFEID) setCondition(status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: by.Generation,
		Reason:             reason,
		Message:            message,
	}
	if previous := meta.FindStatusCondition(by.Status.Conditions, "Ready"); previous != nil && previous.Status == status {
		condition.LastTransitionTime = previous.LastTransitionTime
	}
	meta.SetStatusCondition(&by.NextStatus.Conditions, condition)
}

type ClusterSPIFFEIDSet struct {
	spirev1alpha1.ClusterSPIFFEIDSet
	NextStatus spirev1alpha1.ClusterSPIFFEIDSetStatus
}

func (by *ClusterSPIFFEIDSet) IncrementEntriesToSet() { by.NextStatus.Stats.EntriesToSet++ }

func (by *ClusterSPIFFEIDSet) IncrementEntriesMasked() { by.NextStatus.Stats.EntriesMasked++ }

func (by *ClusterSPIFFEIDSet) IncrementEntrySuccess() {}

func (by *ClusterSPIFFEIDSet) IncrementEntryFailures() {
	by.NextStatus.Stats.EntryFailures++
	by.setCondition(metav1.ConditionFalse, "SPIREAPIError", "Failed to create or update SPIRE entries; see controller logs")
}

func (by *ClusterSPIFFEIDSet) setCondition(status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{Type: "Ready", Status: status, ObservedGeneration: by.Generation, Reason: reason, Message: message}
	if previous := meta.FindStatusCondition(by.Status.Conditions, "Ready"); previous != nil && previous.Status == status {
		condition.LastTransitionTime = previous.LastTransitionTime
	}
	meta.SetStatusCondition(&by.NextStatus.Conditions, condition)
}
