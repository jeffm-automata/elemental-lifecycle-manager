/*
Copyright © 2026 SUSE LLC
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Labels set on every AvailableRelease.
const (
	// CatalogSourceLabel names the ReleaseSource an AvailableRelease was found in.
	CatalogSourceLabel = "lifecycle.suse.com/source"
	// CatalogLineageLabel carries the lineage (the name the release manifest states) of an AvailableRelease.
	CatalogLineageLabel = "lifecycle.suse.com/lineage"
)

// Condition type and reasons of a ReleaseSource.
const (
	// ConditionSourceReady is True when the last query of the source succeeded for every tag.
	ConditionSourceReady = "Ready"
	// SourceSynced: the tag list and every release manifest were read.
	SourceSynced = "Synced"
	// SourceUnreachable: the tag list could not be read. The releases found earlier are kept and marked stale.
	SourceUnreachable = "SourceUnreachable"
	// SourcePartiallySynced: the tag list was read, but at least one release manifest could not be read or parsed.
	SourcePartiallySynced = "PartiallySynced"
	// SourceInvalid: the source cannot be queried as specified (e.g. bad repository or pull secret).
	SourceInvalid = "Invalid"
)

// ReleaseSourceSpec names one OCI repository that holds release manifests, one tag per release.
type ReleaseSourceSpec struct {
	// Registry is the OCI repository of the release manifests, without a tag and without a scheme,
	// e.g. "registry.suse.com/beta/uc/rke2/rke2-manifest". It has the same form and meaning as
	// spec.registry of a Release.
	// +kubebuilder:validation:MinLength=1
	Registry string `json:"registry"`

	// Interval is the time between two queries of the repository. Defaults to 5m; the minimum is 30s.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// PullSecretRef names a Secret of type kubernetes.io/dockerconfigjson in the namespace of the
	// catalog controller, used to authenticate against the registry. Anonymous access when empty.
	// +optional
	PullSecretRef *corev1.LocalObjectReference `json:"pullSecretRef,omitempty"`

	// Insecure allows plain HTTP and skips the verification of the registry's TLS certificate.
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// CABundle is a PEM bundle of additional certificate authorities trusted for this registry.
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`
}

// SkippedTag is a tag of the repository that is not published as an AvailableRelease.
type SkippedTag struct {
	// Tag as listed by the registry.
	Tag string `json:"tag"`
	// Reason the tag is not published.
	Reason string `json:"reason"`
}

// ReleaseSourceStatus is the observed state of a ReleaseSource.
type ReleaseSourceStatus struct {
	// Conditions of the source. Type "Ready".
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the generation the status was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// LastSyncTime is the time of the last query, successful or not.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// LastSuccessfulSyncTime is the time of the last query that read the tag list.
	// +optional
	LastSuccessfulSyncTime *metav1.Time `json:"lastSuccessfulSyncTime,omitempty"`
	// Stale is true when the last query could not read the tag list: the published
	// releases are the last known ones.
	Stale bool `json:"stale"`
	// Releases is the number of AvailableRelease objects published for this source.
	Releases int32 `json:"releases"`
	// Skipped lists tags that are not published, with the reason (at most 50 entries).
	// +optional
	Skipped []SkippedTag `json:"skipped,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=relsrc,categories=elemental
// +kubebuilder:printcolumn:name="Registry",type=string,JSONPath=`.spec.registry`
// +kubebuilder:printcolumn:name="Releases",type=integer,JSONPath=`.status.releases`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Stale",type=boolean,JSONPath=`.status.stale`
// +kubebuilder:printcolumn:name="Last-Success",type=date,JSONPath=`.status.lastSuccessfulSyncTime`

// ReleaseSource is one OCI repository of release manifests that the release catalog queries.
type ReleaseSource struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec ReleaseSourceSpec `json:"spec"`
	// +optional
	Status ReleaseSourceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ReleaseSourceList contains a list of ReleaseSource
type ReleaseSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ReleaseSource `json:"items"`
}

// AvailableOperatingSystem is the operating system a release manifest states.
type AvailableOperatingSystem struct {
	// Image is the operating system image reference (components.operatingSystem.image.base).
	Image string `json:"image"`
	// Version is the tag of that image.
	// +optional
	Version string `json:"version,omitempty"`
}

// AvailableKubernetes is the Kubernetes distribution a release manifest states.
type AvailableKubernetes struct {
	// Version as stated by the manifest, e.g. "v1.37.2+rke2r1".
	Version string `json:"version"`
	// Image is the Kubernetes artifact image reference.
	// +optional
	Image string `json:"image,omitempty"`
}

// AvailableCorePlatform is the core platform release a solution release is pinned to.
type AvailableCorePlatform struct {
	// Image is the reference of the core platform release manifest.
	Image string `json:"image"`
	// Name is the name the core platform manifest states.
	// +optional
	Name string `json:"name,omitempty"`
}

// AvailableReleaseSpec states what the catalog read from one release manifest. It is written by the
// catalog controller only; spec.registry and spec.version are the values to copy into a Release.
type AvailableReleaseSpec struct {
	// Source is the name of the ReleaseSource the release was found in.
	Source string `json:"source"`
	// Registry is the OCI repository of the release manifest (Release.spec.registry).
	Registry string `json:"registry"`
	// Version is the release version, the tag of the manifest (Release.spec.version).
	Version string `json:"version"`
	// Digest of the release manifest artifact the fields below were read from.
	Digest string `json:"digest"`
	// Manifest is the reference of the release manifest pinned by digest: <registry>@<digest>.
	Manifest string `json:"manifest"`
	// Lineage is the name the release manifest states (metadata.name), e.g. "suse-core" or the
	// name of a solution. A cluster can only move between releases of one lineage.
	Lineage string `json:"lineage"`
	// CreationDate as stated by the manifest.
	// +optional
	CreationDate string `json:"creationDate,omitempty"`
	// OperatingSystem of the release.
	OperatingSystem AvailableOperatingSystem `json:"operatingSystem"`
	// Kubernetes of the release.
	Kubernetes AvailableKubernetes `json:"kubernetes"`
	// CorePlatform is set for a solution release.
	// +optional
	CorePlatform *AvailableCorePlatform `json:"corePlatform,omitempty"`
}

// AvailableReleaseStatus is the observed state of an AvailableRelease.
type AvailableReleaseStatus struct {
	// DiscoveredAt is the time the catalog first read this manifest digest.
	// +optional
	DiscoveredAt *metav1.Time `json:"discoveredAt,omitempty"`
	// Stale is true when the last query of the source could not confirm that the release
	// is still there: the entry is the last known one.
	Stale bool `json:"stale"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=avrel,categories=elemental
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Lineage",type=string,JSONPath=`.spec.lineage`
// +kubebuilder:printcolumn:name="Kubernetes",type=string,JSONPath=`.spec.kubernetes.version`
// +kubebuilder:printcolumn:name="OS",type=string,JSONPath=`.spec.operatingSystem.version`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="Stale",type=boolean,JSONPath=`.status.stale`
// +kubebuilder:printcolumn:name="Found",type=date,JSONPath=`.status.discoveredAt`
// +kubebuilder:printcolumn:name="Registry",type=string,JSONPath=`.spec.registry`,priority=1
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.spec.digest`,priority=1

// AvailableRelease is one release found in a ReleaseSource. Read-only for users.
type AvailableRelease struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec AvailableReleaseSpec `json:"spec"`
	// +optional
	Status AvailableReleaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AvailableReleaseList contains a list of AvailableRelease
type AvailableReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AvailableRelease `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion,
			&ReleaseSource{}, &ReleaseSourceList{},
			&AvailableRelease{}, &AvailableReleaseList{})
		return nil
	})
}
