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

package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// registry answers like an OCI registry for one repository "rel/manifest" with tag "1.0".
func registry(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case status != 0:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"errors":[{"code":"TEST","message":"test"}]}`))
		case r.URL.Path == "/v2/rel/manifest/manifests/1.0":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Length", "2")
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte("{}"))
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func kindOf(t *testing.T, err error) Kind {
	t.Helper()
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("error %v is not a *release.Error", err)
	}
	return re.Kind
}

func TestResolveTag(t *testing.T) {
	host := registry(t, 0)
	for _, ref := range []string{host + "/rel/manifest:1.0", "oci://" + host + "/rel/manifest:1.0"} {
		got, err := Registry{}.Resolve(context.Background(), ref)
		if err != nil || got != digest {
			t.Fatalf("Resolve(%s) = %q, %v", ref, got, err)
		}
	}
}

func TestMissingReleaseIsTerminal(t *testing.T) {
	host := registry(t, 0)
	_, err := Registry{}.Resolve(context.Background(), host+"/rel/manifest:9.9")
	if k := kindOf(t, err); k != KindNotFound {
		t.Fatalf("kind = %s, want %s", k, KindNotFound)
	}
	var re *Error
	errors.As(err, &re)
	if !re.Terminal() {
		t.Fatal("a missing release must be terminal")
	}
}

func TestClassification(t *testing.T) {
	cases := map[int]struct {
		kind     Kind
		terminal bool
	}{
		http.StatusNotFound:            {KindNotFound, true},
		http.StatusUnauthorized:        {KindDenied, true},
		http.StatusForbidden:           {KindDenied, true},
		http.StatusInternalServerError: {KindTransient, false},
		http.StatusServiceUnavailable:  {KindTransient, false},
		http.StatusTooManyRequests:     {KindTransient, false},
	}
	for status, want := range cases {
		host := registry(t, status)
		_, err := Registry{}.Resolve(context.Background(), host+"/rel/manifest:1.0")
		var re *Error
		if !errors.As(err, &re) {
			t.Fatalf("status %d: error %v is not classified", status, err)
		}
		if re.Kind != want.kind || re.Terminal() != want.terminal {
			t.Errorf("status %d: kind %s terminal %v, want %s %v", status, re.Kind, re.Terminal(), want.kind, want.terminal)
		}
	}
}

func TestUnreachableRegistryIsRetried(t *testing.T) {
	_, err := Registry{}.Resolve(context.Background(), "127.0.0.1:1/rel/manifest:1.0")
	if k := kindOf(t, err); k != KindTransient {
		t.Fatalf("kind = %s, want %s", k, KindTransient)
	}
}

func TestInvalidReference(t *testing.T) {
	for _, ref := range []string{"", "no-registry-or-tag", "registry.example/Upper:1", "registry.example/repo"} {
		_, err := Registry{}.Resolve(context.Background(), ref)
		if k := kindOf(t, err); k != KindInvalid {
			t.Errorf("%q: kind = %s, want %s", ref, k, KindInvalid)
		}
	}
}
