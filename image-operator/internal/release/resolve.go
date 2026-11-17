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

// Package release resolves a release manifest reference to its digest and tells errors that
// will not go away (the release does not exist) from errors worth retrying.
package release

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Kind classifies a resolve error.
type Kind string

const (
	// KindInvalid: the reference can not be parsed. Terminal.
	KindInvalid Kind = "InvalidSpec"
	// KindNotFound: the registry says the repository or manifest does not exist. Terminal.
	KindNotFound Kind = "ReleaseNotFound"
	// KindDenied: the registry refuses anonymous access. Terminal (no pull secret support yet).
	KindDenied Kind = "ReleaseAccessDenied"
	// KindTransient: anything else (network, server errors). Retried.
	KindTransient Kind = "Resolving"
)

// Error is a classified resolve error.
type Error struct {
	Kind Kind
	Ref  string
	Err  error
}

func (e *Error) Error() string {
	switch e.Kind {
	case KindNotFound:
		return fmt.Sprintf("release %s does not exist at the registry: %v", e.Ref, e.Err)
	case KindDenied:
		return fmt.Sprintf("the registry refuses anonymous access to release %s: %v", e.Ref, e.Err)
	}
	return fmt.Sprintf("release %s: %v", e.Ref, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

// Terminal says whether retrying can help.
func (e *Error) Terminal() bool { return e.Kind != KindTransient }

// Resolver turns a reference into a digest.
type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Registry resolves against the registry named in the reference, anonymously.
type Registry struct {
	// Options are added to every request; tests use them.
	Options []remote.Option
}

// Resolve returns "sha256:<hex>" for a reference by tag or digest. Errors are always *Error.
func (r Registry) Resolve(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimPrefix(ref, "oci://")
	parsed, err := name.ParseReference(ref, name.StrictValidation)
	if err != nil {
		return "", &Error{Kind: KindInvalid, Ref: ref, Err: fmt.Errorf("not a full OCI reference (registry/repository:tag or @digest): %w", err)}
	}
	opts := append([]remote.Option{remote.WithContext(ctx)}, r.Options...)
	desc, err := remote.Head(parsed, opts...)
	if err != nil {
		return "", &Error{Kind: classify(err), Ref: ref, Err: err}
	}
	return desc.Digest.String(), nil
}

func classify(err error) Kind {
	if terr, ok := errors.AsType[*transport.Error](err); ok {
		switch terr.StatusCode {
		case http.StatusNotFound:
			return KindNotFound
		case http.StatusUnauthorized, http.StatusForbidden:
			return KindDenied
		}
	}
	return KindTransient
}
