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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Finalizer lets the operator remove builders (and, with deletionPolicy Delete, the image) on the target.
	Finalizer = "image.lifecycle.suse.com/cleanup"

	// TargetHarvester is the only target type implemented.
	TargetHarvester = "harvester"

	// CredentialsKey is the key of the target credentials Secret that holds a kubeconfig.
	CredentialsKey = "kubeconfig"
)

// Builder says where the build of an image runs.
// +kubebuilder:validation:Enum=vm;pod
type Builder string

const (
	// BuilderVM is a short-lived virtual machine on the target: the build is isolated by the VM.
	BuilderVM Builder = "vm"
	// BuilderPod is a short-lived pod on a Kubernetes cluster the operator is configured with.
	BuilderPod Builder = "pod"
)

// Phase is a one-word summary of the conditions.
type Phase string

const (
	PhasePending    Phase = "Pending"
	PhaseBuilding   Phase = "Building"
	PhaseDelivering Phase = "Delivering"
	PhaseReady      Phase = "Ready"
	PhaseFailed     Phase = "Failed"
)

// Condition types.
const (
	ConditionResolved  = "Resolved"
	ConditionBuilt     = "Built"
	ConditionDelivered = "Delivered"
	ConditionReady     = "Ready"
)

// Condition reasons.
const (
	ReasonResolving           = "Resolving"
	ReasonResolved            = "Resolved"
	ReasonBuilding            = "Building"
	ReasonBuilt               = "Built"
	ReasonDelivering          = "Delivering"
	ReasonDelivered           = "Delivered"
	ReasonCacheHit            = "CacheHit"
	ReasonReady               = "Ready"
	ReasonTargetUnavailable   = "TargetUnavailable"
	ReasonReleaseNotFound     = "ReleaseNotFound"
	ReasonReleaseAccessDenied = "ReleaseAccessDenied"
	ReasonInvalidSpec         = "InvalidSpec"
	ReasonUnsupportedTarget   = "UnsupportedTarget"
	ReasonUnsupportedBuilder  = "UnsupportedBuilder"
	ReasonCredentialsInvalid  = "CredentialsInvalid"
	ReasonBuildFailed         = "BuildFailed"
	ReasonImageRejected       = "ImageRejected"
	ReasonTargetRejected      = "TargetRejected"
)

// DeletionPolicy says what happens to the image on the target when the last request for it is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	DeletionRetain DeletionPolicy = "Retain"
	DeletionDelete DeletionPolicy = "Delete"
)

// ReleaseSpec names the release the image is built from.
type ReleaseSpec struct {
	// Manifest is the OCI reference of a release manifest, by tag or by digest, without a scheme.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`
	Manifest string `json:"manifest"`
}

// HarvesterTarget says where on a Harvester cluster the image goes.
type HarvesterTarget struct {
	// Namespace of the image, and of the short-lived builder VM.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// StorageClassName is the storage class volumes created from the image use.
	// +kubebuilder:default=harvester-longhorn
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`
	StorageClassName string `json:"storageClassName,omitempty"`
	// NamePrefix is put in front of every object the operator creates on the target.
	// +kubebuilder:default=elemental-
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,30}-)?$`
	NamePrefix string `json:"namePrefix,omitempty"`
}

// TargetSpec says what the image is for and where it is delivered.
// +kubebuilder:validation:XValidation:rule="self.type != 'harvester' || has(self.harvester)",message="target type harvester needs the harvester section"
type TargetSpec struct {
	// Type of the target. Only "harvester" is implemented.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	Type string `json:"type"`
	// CredentialsSecretRef names a Secret in the namespace of the NodeImage whose key "kubeconfig"
	// gives access to the target.
	CredentialsSecretRef SecretRef `json:"credentialsSecretRef"`
	// Harvester holds the settings of target type "harvester".
	// +optional
	Harvester *HarvesterTarget `json:"harvester,omitempty"`
}

// SecretRef names a Secret in the same namespace.
type SecretRef struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`
	Name string `json:"name"`
}

// ChartValues are Helm values for one chart of the release.
type ChartValues struct {
	// Chart is the chart name as the release manifest lists it.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]{0,61}[a-z0-9])?$`
	Chart string `json:"chart"`
	// Values is a YAML document of Helm values.
	// +kubebuilder:validation:MaxLength=16384
	Values string `json:"values"`
}

// ConfigSpec is everything a requester may change about the content of the image. All of it is part
// of the cache key.
type ConfigSpec struct {
	// DiskSize is the virtual size of the image; node disks must be at least this big.
	// +kubebuilder:default="20G"
	// +kubebuilder:validation:Pattern=`^[1-9][0-9]{0,4}[MG]$`
	DiskSize string `json:"diskSize,omitempty"`
	// ChartValues gives Helm values for charts of the release.
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=chart
	// +optional
	ChartValues []ChartValues `json:"chartValues,omitempty"`
}

// NodeImageSpec asks for the node image of one release on one target. It can not be changed:
// a different release, target or configuration is a different NodeImage.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new NodeImage"
type NodeImageSpec struct {
	Release ReleaseSpec `json:"release"`
	// Arch is the machine architecture of the image. Only x86_64 has been run.
	// +kubebuilder:default=x86_64
	// +kubebuilder:validation:Enum=x86_64;aarch64
	Arch   string     `json:"arch,omitempty"`
	Target TargetSpec `json:"target"`
	// +kubebuilder:default={}
	Config ConfigSpec `json:"config,omitempty"`
	// Builder says where the build runs when the image is not on the target yet. Empty: the
	// operator's default (its --builder flag). It does not change the image and is not part of
	// the cache key.
	// +optional
	Builder Builder `json:"builder,omitempty"`
	// +kubebuilder:default=Retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// ReleaseStatus is the release as resolved at the registry.
type ReleaseStatus struct {
	ManifestDigest string       `json:"manifestDigest,omitempty"`
	ResolvedAt     *metav1.Time `json:"resolvedAt,omitempty"`
}

// ImageStatus describes the image on the target. For Harvester, Namespace/Name is what a machine
// template refers to.
type ImageStatus struct {
	Name        string `json:"name,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	// KubernetesVersion is the version in the image, as a control plane object must state it.
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
}

// BuildStatus is the history of builders this request had. It is absent on a cache hit.
type BuildStatus struct {
	// Builder is the name of the current or last builder.
	Builder string `json:"builder,omitempty"`
	// Backend is where that builder ran: vm or pod.
	Backend Builder `json:"backend,omitempty"`
	// Attempts counts failed builds or deliveries.
	Attempts   int32        `json:"attempts,omitempty"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// LastError is the last failure, with the end of the build log when there is one.
	LastError string `json:"lastError,omitempty"`
}

// NodeImageStatus is what the operator observed.
type NodeImageStatus struct {
	Phase              Phase  `json:"phase,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	CacheKey           string `json:"cacheKey,omitempty"`
	// CacheHit is true when the image was found on the target and this request never had a builder.
	CacheHit *bool          `json:"cacheHit,omitempty"`
	Release  *ReleaseStatus `json:"release,omitempty"`
	Image    *ImageStatus   `json:"image,omitempty"`
	Build    *BuildStatus   `json:"build,omitempty"`
	// ReadyAt is when the image first became usable for this request.
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NodeImage asks for the node image of a release to exist on a target.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="CacheHit",type=boolean,JSONPath=`.status.cacheHit`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image.name`
// +kubebuilder:printcolumn:name="Kubernetes",type=string,JSONPath=`.status.image.kubernetesVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type NodeImage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeImageSpec   `json:"spec"`
	Status NodeImageStatus `json:"status,omitempty"`
}

// NodeImageList is a list of NodeImage.
// +kubebuilder:object:root=true
type NodeImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeImage `json:"items"`
}
