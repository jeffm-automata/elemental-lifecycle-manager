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

package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1 "github.com/suse/elemental-lifecycle-manager/image-operator/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/builder/pod"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target/harvester"
)

func TestChooseBuilder(t *testing.T) {
	for _, c := range []struct {
		requested, def imagev1.Builder
		podConfigured  bool
		want           imagev1.Builder
	}{
		{"", imagev1.BuilderVM, false, imagev1.BuilderVM},
		{"", imagev1.BuilderVM, true, imagev1.BuilderVM},
		{"", imagev1.BuilderPod, true, imagev1.BuilderPod},
		{imagev1.BuilderPod, imagev1.BuilderVM, true, imagev1.BuilderPod},
		{imagev1.BuilderVM, imagev1.BuilderPod, true, imagev1.BuilderVM},
		// Never the other builder in place of the one asked for.
		{imagev1.BuilderPod, imagev1.BuilderVM, false, ""},
		{"", imagev1.BuilderPod, false, ""},
		{"", "", true, ""},
		{"container", imagev1.BuilderVM, true, ""},
	} {
		got, err := ChooseBuilder(c.requested, c.def, c.podConfigured)
		if got != c.want || (err == nil) != (c.want != "") {
			t.Errorf("ChooseBuilder(%q, %q, %v) = %q, %v; want %q", c.requested, c.def, c.podConfigured, got, err, c.want)
		}
	}
}

// A kubeconfig that parses; no connection is made when clients are created.
const testKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`

func TestBackendSelection(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "creds"}, Data: map[string][]byte{imagev1.CredentialsKey: []byte(testKubeconfig)}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	podBuilder := &PodBuilder{Client: reader, Config: pod.Config{Namespace: "builders", Image: "img"}}
	request := func(b imagev1.Builder) *imagev1.NodeImage {
		ni := nodeImage("r")
		ni.Spec.Builder = b
		return ni
	}
	for _, c := range []struct {
		name      string
		def, spec imagev1.Builder
		pod       *PodBuilder
		want      string // a builder or a reason
	}{
		{"default vm", imagev1.BuilderVM, "", podBuilder, harvester.Backend},
		{"default pod", imagev1.BuilderPod, "", podBuilder, pod.Backend},
		{"request overrides the default with pod", imagev1.BuilderVM, imagev1.BuilderPod, podBuilder, pod.Backend},
		{"request overrides the default with vm", imagev1.BuilderPod, imagev1.BuilderVM, podBuilder, harvester.Backend},
		{"pod requested, none configured", imagev1.BuilderVM, imagev1.BuilderPod, nil, imagev1.ReasonUnsupportedBuilder},
	} {
		s := &SecretBackends{Reader: reader, DefaultBuilder: c.def, Pod: c.pod}
		tgt, bld, err := s.For(context.Background(), request(c.spec))
		if err != nil {
			be, ok := errors.AsType[*BackendError](err)
			if !ok || !be.Terminal || be.Reason != c.want {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if _, ok := tgt.(*harvester.Harvester); !ok {
			t.Errorf("%s: the target is always the Harvester of the request, got %T", c.name, tgt)
		}
		switch b := bld.(type) {
		case *harvester.Harvester:
			if c.want != harvester.Backend {
				t.Errorf("%s: got the VM builder", c.name)
			}
		case *pod.Pod:
			// The pod builder runs where the operator is configured to, with the request's name prefix.
			if c.want != pod.Backend || b.Config.Namespace != "builders" || b.Prefix != "p-" || b.Client != podBuilder.Client {
				t.Errorf("%s: got the pod builder %+v", c.name, b)
			}
		default:
			t.Errorf("%s: builder %T", c.name, bld)
		}
	}
}
