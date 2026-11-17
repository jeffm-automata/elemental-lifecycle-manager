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

// Package crd checks the CustomResourceDefinition against a real API server (envtest): defaults,
// patterns and the immutability rule live in the schema, not in Go code, so only an API server
// can test them. Skipped unless KUBEBUILDER_ASSETS points at envtest binaries (make test-crd).
package crd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	imagev1 "github.com/suse/elemental-lifecycle-manager/image-operator/api/v1alpha1"
)

func minimal(name string) *imagev1.NodeImage {
	return &imagev1.NodeImage{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: imagev1.NodeImageSpec{
			Release: imagev1.ReleaseSpec{Manifest: "registry.example/rel/manifest:1.0"},
			Target: imagev1.TargetSpec{
				Type: imagev1.TargetHarvester, CredentialsSecretRef: imagev1.SecretRef{Name: "creds"},
				Harvester: &imagev1.HarvesterTarget{Namespace: "tenant"},
			},
		},
	}
}

func start(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test-crd")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err := imagev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSchema(t *testing.T) {
	c := start(t)
	ctx := context.Background()

	// Defaults.
	ni := minimal("defaults")
	if err := c.Create(ctx, ni); err != nil {
		t.Fatal(err)
	}
	h := ni.Spec.Target.Harvester
	if ni.Spec.Arch != "x86_64" || ni.Spec.Config.DiskSize != "20G" || ni.Spec.DeletionPolicy != imagev1.DeletionRetain ||
		h.StorageClassName != "harvester-longhorn" || h.NamePrefix != "elemental-" {
		t.Fatalf("defaults not applied: %+v %+v", ni.Spec, *h)
	}

	// The spec can not be changed; the status and metadata can.
	changed := ni.DeepCopy()
	changed.Spec.Config.DiskSize = "30G"
	if err := c.Update(ctx, changed); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("spec change was not refused as immutable: %v", err)
	}
	ni.Labels = map[string]string{"a": "b"}
	if err := c.Update(ctx, ni); err != nil {
		t.Fatalf("label change refused: %v", err)
	}
	ni.Status.Phase = imagev1.PhaseBuilding
	ni.Status.CacheKey = strings.Repeat("a", 64)
	if err := c.Status().Update(ctx, ni); err != nil {
		t.Fatalf("status update refused: %v", err)
	}

	// A target type that is not implemented passes the schema (the controller reports it);
	// two values entries for one chart do not.
	other := minimal("other-target")
	other.Spec.Target.Type, other.Spec.Target.Harvester = "vsphere", nil
	if err := c.Create(ctx, other); err != nil {
		t.Errorf("an unknown target type must be left to the controller: %v", err)
	}
	dup := minimal("dup")
	dup.Spec.Config.ChartValues = []imagev1.ChartValues{{Chart: "a", Values: "x: 1"}, {Chart: "a", Values: "x: 2"}}
	if err := c.Create(ctx, dup); err == nil {
		t.Error("two values entries for one chart were accepted")
	}
}

// Values that would reach a shell in the builder, or that name things on the target, are refused
// by the schema before the controller ever sees them.
func TestSchemaRefusesUnsafeValues(t *testing.T) {
	c := start(t)
	bad := map[string]func(*imagev1.NodeImage){
		"manifest with a quote":      func(n *imagev1.NodeImage) { n.Spec.Release.Manifest = "registry.example/a:1'; id; '" },
		"manifest with a space":      func(n *imagev1.NodeImage) { n.Spec.Release.Manifest = "registry.example/a:1 b" },
		"manifest with a subshell":   func(n *imagev1.NodeImage) { n.Spec.Release.Manifest = "$" + "(id)" },
		"empty manifest":             func(n *imagev1.NodeImage) { n.Spec.Release.Manifest = "" },
		"unknown architecture":       func(n *imagev1.NodeImage) { n.Spec.Arch = "riscv64" },
		"disk size with a command":   func(n *imagev1.NodeImage) { n.Spec.Config.DiskSize = "20G;id" },
		"disk size without a unit":   func(n *imagev1.NodeImage) { n.Spec.Config.DiskSize = "20" },
		"harvester without settings": func(n *imagev1.NodeImage) { n.Spec.Target.Harvester = nil },
		"prefix without a dash":      func(n *imagev1.NodeImage) { n.Spec.Target.Harvester.NamePrefix = "abc" },
		"upper-case namespace":       func(n *imagev1.NodeImage) { n.Spec.Target.Harvester.Namespace = "Tenant" },
		"unknown deletion policy":    func(n *imagev1.NodeImage) { n.Spec.DeletionPolicy = "Orphan" },
		"secret name with a slash":   func(n *imagev1.NodeImage) { n.Spec.Target.CredentialsSecretRef.Name = "a/b" },
		"chart name with a path": func(n *imagev1.NodeImage) {
			n.Spec.Config.ChartValues = []imagev1.ChartValues{{Chart: "../x", Values: ""}}
		},
	}
	for name, change := range bad {
		n := minimal("bad")
		change(n)
		if err := c.Create(context.Background(), n); err == nil {
			t.Errorf("%s: accepted", name)
			_ = c.Delete(context.Background(), n)
		}
	}
}
