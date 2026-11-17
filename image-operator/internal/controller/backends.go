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
	"crypto/sha256"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1 "github.com/suse/elemental-lifecycle-manager/image-operator/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target/harvester"
)

// SecretBackends builds targets from the credentials Secret a NodeImage names. The Secret is
// read directly (no cache, no watch) and its content is never logged or stored; only clients made
// from it are kept, indexed by a hash of the kubeconfig.
type SecretBackends struct {
	// Reader reads Secrets without a cache.
	Reader  client.Reader
	Builder harvester.BuilderConfig

	mu      sync.Mutex
	clients map[[32]byte]*targetClients
}

type targetClients struct {
	c    client.Client
	core kubernetes.Interface
}

// For implements Backends.
func (s *SecretBackends) For(ctx context.Context, ni *imagev1.NodeImage) (target.Target, target.Builder, error) {
	if ni.Spec.Target.Type != imagev1.TargetHarvester || ni.Spec.Target.Harvester == nil {
		return nil, nil, &BackendError{Reason: imagev1.ReasonUnsupportedTarget, Terminal: true, Err: fmt.Errorf("target type %q is not implemented", ni.Spec.Target.Type)}
	}
	name := ni.Spec.Target.CredentialsSecretRef.Name
	secret := &corev1.Secret{}
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ni.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Not terminal: the Secret may simply be created after the request.
			return nil, nil, &BackendError{Reason: imagev1.ReasonCredentialsInvalid, Err: fmt.Errorf("credentials Secret %s/%s does not exist", ni.Namespace, name)}
		}
		return nil, nil, err
	}
	kubeconfig := secret.Data[imagev1.CredentialsKey]
	if len(kubeconfig) == 0 {
		return nil, nil, &BackendError{Reason: imagev1.ReasonCredentialsInvalid, Terminal: true, Err: fmt.Errorf("credentials Secret %s/%s has no key %q", ni.Namespace, name, imagev1.CredentialsKey)}
	}
	tc, err := s.clientsFor(kubeconfig)
	if err != nil {
		// The error text of a kubeconfig parser can quote the input; do not pass it on.
		return nil, nil, &BackendError{Reason: imagev1.ReasonCredentialsInvalid, Terminal: true, Err: fmt.Errorf("credentials Secret %s/%s does not hold a usable kubeconfig", ni.Namespace, name)}
	}
	ht := ni.Spec.Target.Harvester
	h := &harvester.Harvester{
		Client: tc.c, Namespace: ht.Namespace, StorageClass: ht.StorageClassName, Prefix: ht.NamePrefix, Builder: s.Builder,
		Fetch: func(ctx context.Context, namespace, service, path string) ([]byte, error) {
			return tc.core.CoreV1().Services(namespace).ProxyGet("http", service, "8080", path, nil).DoRaw(ctx)
		},
	}
	return h, h, nil
}

func (s *SecretBackends) clientsFor(kubeconfig []byte) (*targetClients, error) {
	sum := sha256.Sum256(kubeconfig)
	s.mu.Lock()
	defer s.mu.Unlock()
	if tc, ok := s.clients[sum]; ok {
		return tc, nil
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "elemental-image-operator"
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, err
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	if s.clients == nil {
		s.clients = map[[32]byte]*targetClients{}
	}
	tc := &targetClients{c: c, core: core}
	s.clients[sum] = tc
	return tc, nil
}
