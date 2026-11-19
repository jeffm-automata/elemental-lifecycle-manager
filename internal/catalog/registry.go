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

// Package catalog implements the release catalog: it lists the release manifests of OCI
// repositories and publishes them as AvailableRelease objects.
package catalog

import (
	"archive/tar"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ManifestPath is the path of the release manifest file inside a release manifest image.
const ManifestPath = "release_manifest.yaml"

// maxManifestSize bounds what is read from a registry for one manifest file.
const maxManifestSize = 4 << 20

// Access describes how a repository is reached.
type Access struct {
	// Insecure allows plain HTTP and skips TLS verification.
	Insecure bool
	// CABundle is an additional PEM bundle of trusted certificate authorities.
	CABundle []byte
	// DockerConfigJSON is the content of a kubernetes.io/dockerconfigjson Secret, or nil.
	DockerConfigJSON []byte
}

// Registry is what the catalog needs from an OCI registry.
type Registry interface {
	// ListTags returns the tags of the repository.
	ListTags(ctx context.Context, repository string, access Access) ([]string, error)
	// Digest returns the digest the tag points to.
	Digest(ctx context.Context, repository, tag string, access Access) (string, error)
	// ReadManifest returns the release manifest file of the image at ref (repo:tag or repo@digest).
	ReadManifest(ctx context.Context, ref string, access Access) ([]byte, error)
}

// RemoteRegistry talks to real registries.
type RemoteRegistry struct{}

var _ Registry = RemoteRegistry{}

// CleanRepository strips an "oci://" scheme and surrounding slashes from a repository reference.
func CleanRepository(repository string) string {
	return strings.Trim(strings.TrimPrefix(strings.TrimSpace(repository), "oci://"), "/")
}

func nameOptions(access Access) []name.Option {
	if access.Insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

func remoteOptions(ctx context.Context, registryHost string, access Access) ([]remote.Option, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if access.Insecure {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // asked for by spec.insecure of the source
	}
	if len(access.CABundle) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(access.CABundle) {
			return nil, errors.New("caBundle holds no PEM certificate")
		}
		tlsConfig.RootCAs = pool
	}
	transport.TLSClientConfig = tlsConfig

	auth := authn.Anonymous
	if len(access.DockerConfigJSON) > 0 {
		a, err := authFromDockerConfig(access.DockerConfigJSON, registryHost)
		if err != nil {
			return nil, err
		}
		auth = a
	}

	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithTransport(transport),
		remote.WithAuth(auth),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
	}, nil
}

// authFromDockerConfig returns the credentials a docker config.json holds for the registry host.
// No entry for the host means anonymous access.
func authFromDockerConfig(data []byte, registryHost string) (authn.Authenticator, error) {
	var cfg struct {
		Auths map[string]struct {
			Username      string `json:"username"`
			Password      string `json:"password"`
			Auth          string `json:"auth"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, errors.New("pull secret is not a docker config.json")
	}
	for host, entry := range cfg.Auths {
		h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
		if h != registryHost {
			continue
		}
		ac := authn.AuthConfig{Username: entry.Username, Password: entry.Password, IdentityToken: entry.IdentityToken}
		if ac.Username == "" && entry.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				return nil, fmt.Errorf("pull secret entry for %s is not valid base64", registryHost)
			}
			user, pass, ok := strings.Cut(string(raw), ":")
			if !ok {
				return nil, fmt.Errorf("pull secret entry for %s is not user:password", registryHost)
			}
			ac.Username, ac.Password = user, pass
		}
		return authn.FromConfig(ac), nil
	}
	return authn.Anonymous, nil
}

// ListTags implements Registry.
func (RemoteRegistry) ListTags(ctx context.Context, repository string, access Access) ([]string, error) {
	repo, err := name.NewRepository(CleanRepository(repository), nameOptions(access)...)
	if err != nil {
		return nil, &InvalidSourceError{Err: fmt.Errorf("parsing repository %q: %w", repository, err)}
	}
	opts, err := remoteOptions(ctx, repo.RegistryStr(), access)
	if err != nil {
		return nil, &InvalidSourceError{Err: err}
	}
	tags, err := remote.List(repo, opts...)
	if err != nil {
		return nil, fmt.Errorf("listing tags of %s: %w", repo.Name(), err)
	}
	return tags, nil
}

// Digest implements Registry.
func (RemoteRegistry) Digest(ctx context.Context, repository, tag string, access Access) (string, error) {
	ref, err := name.NewTag(CleanRepository(repository)+":"+tag, nameOptions(access)...)
	if err != nil {
		return "", fmt.Errorf("parsing reference: %w", err)
	}
	opts, err := remoteOptions(ctx, ref.RegistryStr(), access)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(ref, opts...)
	if err != nil {
		// Some registries do not answer HEAD; GET is the fallback.
		full, getErr := remote.Get(ref, opts...)
		if getErr != nil {
			return "", fmt.Errorf("reading digest of %s: %w", ref.Name(), getErr)
		}
		return full.Digest.String(), nil
	}
	return desc.Digest.String(), nil
}

// ReadManifest implements Registry.
func (RemoteRegistry) ReadManifest(ctx context.Context, imageRef string, access Access) ([]byte, error) {
	ref, err := name.ParseReference(CleanRepository(imageRef), nameOptions(access)...)
	if err != nil {
		return nil, fmt.Errorf("parsing reference %q: %w", imageRef, err)
	}
	opts, err := remoteOptions(ctx, ref.Context().RegistryStr(), access)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", ref.Name(), err)
	}

	rc := mutate.Extract(img)
	defer func() { _ = rc.Close() }()

	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, &NotAReleaseError{Err: fmt.Errorf("no %s in %s", ManifestPath, ref.Name())}
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", ref.Name(), err)
		}
		if strings.TrimPrefix(header.Name, "./") != ManifestPath {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxManifestSize+1))
		if err != nil {
			return nil, fmt.Errorf("reading %s from %s: %w", ManifestPath, ref.Name(), err)
		}
		if len(data) > maxManifestSize {
			return nil, &NotAReleaseError{Err: fmt.Errorf("%s in %s is larger than %d bytes", ManifestPath, ref.Name(), maxManifestSize)}
		}
		return data, nil
	}
}

// InvalidSourceError marks an error that querying again will not cure.
type InvalidSourceError struct{ Err error }

func (e *InvalidSourceError) Error() string { return e.Err.Error() }
func (e *InvalidSourceError) Unwrap() error { return e.Err }

// NotAReleaseError marks an artifact that was read but is not a usable release manifest.
type NotAReleaseError struct{ Err error }

func (e *NotAReleaseError) Error() string { return e.Err.Error() }
func (e *NotAReleaseError) Unwrap() error { return e.Err }
