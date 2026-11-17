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

// Package target defines what the operator needs from the place an image is built at and
// delivered to. One implementation exists (harvester); the controller knows only these interfaces.
package target

import (
	"context"
	"time"
)

// ImageState is the state of the image for a key on the target.
type ImageState string

const (
	ImageAbsent    ImageState = "Absent"
	ImageImporting ImageState = "Importing"
	ImageReady     ImageState = "Ready"
	ImageFailed    ImageState = "Failed"
)

// BuildState is the state of the builder for a key.
type BuildState string

const (
	BuildAbsent    BuildState = "Absent"
	BuildRunning   BuildState = "Running"
	BuildSucceeded BuildState = "Succeeded"
	BuildFailed    BuildState = "Failed"
)

// ChartValues are Helm values for one chart of the release.
type ChartValues struct {
	Chart  string
	Values string
}

// Request is one image, identified by Key. The other fields are the inputs the key was made from
// and where the image goes.
type Request struct {
	Key            string
	ManifestRef    string
	ManifestDigest string
	Arch           string
	DiskSize       string
	ChartValues    []ChartValues
}

// Image is the image for a key as found on the target.
type Image struct {
	State             ImageState
	Name              string
	Namespace         string
	DisplayName       string
	SHA256            string
	KubernetesVersion string
	// Created is when the target object was created; delivery deadlines count from here.
	Created time.Time
	Message string
}

// Artifact is a finished build, at a place the target can fetch it from.
type Artifact struct {
	URL               string
	File              string
	SHA256            string
	SHA512            string
	SizeBytes         int64
	KubernetesVersion string
}

// Build is the builder for a key.
type Build struct {
	State BuildState
	Name  string
	// Created is when the builder was created; build deadlines count from here.
	Created time.Time
	// Step is what a running builder is doing.
	Step string
	// Message describes a failure, with the end of the build log when there is one.
	Message string
	// Terminal: a failure that a new attempt can not fix (bad arguments, image rejected by inspection).
	Terminal bool
	Artifact *Artifact
}

// Target stores images and is asked for them by key.
type Target interface {
	// FindImage looks the image for the key up. No image is State ImageAbsent, not an error.
	FindImage(ctx context.Context, req Request) (Image, error)
	// Deliver makes the artifact an image on the target. It returns once the target has
	// accepted the request; FindImage reports progress. Idempotent.
	Deliver(ctx context.Context, req Request, a Artifact) error
	// DeleteImage removes the image for the key. Idempotent.
	DeleteImage(ctx context.Context, req Request) error
}

// Builder runs the build recipe somewhere the target can fetch the result from.
type Builder interface {
	// Status reports the builder for the key. No builder is State BuildAbsent, not an error.
	Status(ctx context.Context, req Request) (Build, error)
	// Ensure creates the builder or moves one that is still being set up one step on. Idempotent.
	Ensure(ctx context.Context, req Request) error
	// Cleanup removes everything Ensure created. Idempotent.
	Cleanup(ctx context.Context, req Request) error
}
