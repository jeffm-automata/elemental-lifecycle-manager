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

package catalog

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
)

const (
	repoPath   = "e3p/rke2-manifest"
	suseCore   = "suse-core"
	k8s1372    = "v1.37.2+rke2r1"
	harvesterL = "harvester"
)

func coreManifest(k8s, osTag string) string {
	return fmt.Sprintf(`schema: v1
metadata:
  name: %s
  creationDate: '2026-11-18'
  elemental:
    version: '3.1'
    image: registry.example/elemental:3.1.0
components:
  operatingSystem:
    image:
      base: registry.example:5000/os/base-os-kernel-default:%s
      iso: registry.example/os/base-os-kernel-default-iso:16.1-76.81
  kubernetes:
    version: %s
    image: registry.example/rke2-tar:1.37.2_rke2r1-8.1
`, suseCore, osTag, k8s)
}

const osOnlyManifest = `schema: v1
metadata:
  name: suse-core
  elemental:
    version: '3.1'
components:
  operatingSystem:
    image:
      base: registry.example/os/base-os-kernel-default:16.1-76.43
      iso: registry.example/os/base-os-kernel-default-iso:16.1-76.81
`

func solutionManifest(lineage, coreRef string) string {
	return fmt.Sprintf(`schema: v1
metadata:
  name: %s
  creationDate: '2026-11-19'
corePlatform:
  image: %s
`, lineage, coreRef)
}

// fakeRegistry is an in-memory OCI registry served over HTTP.
type fakeRegistry struct {
	server *httptest.Server
	host   string
	down   bool
	auth   string // when set, the Authorization header every request must carry
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{}
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if f.auth != "" && r.Header.Get("Authorization") != f.auth {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	f.host = strings.TrimPrefix(f.server.URL, "http://")
	return f
}

func (f *fakeRegistry) repo() string { return f.host + "/" + repoPath }

// push stores an image whose only layer holds the given files, and returns its digest.
func (f *fakeRegistry) push(t *testing.T, ref string, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for path, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	opts := []remote.Option{}
	if f.auth != "" {
		opts = append(opts, remote.WithTransport(headerTransport{f.auth}))
	}
	if err = remote.Write(parsed, img, opts...); err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest.String()
}

type headerTransport struct{ auth string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", h.auth)
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fakeRegistry) pushManifest(t *testing.T, tag, manifest string) string {
	t.Helper()
	return f.push(t, f.repo()+":"+tag, map[string]string{ManifestPath: manifest})
}

// --- tag listing ---

func TestListTagsAndDigest(t *testing.T) {
	f := newFakeRegistry(t)
	d1 := f.pushManifest(t, "1.37.1-17.1", coreManifest("v1.37.1+rke2r1", "16.1-70.1"))
	f.pushManifest(t, "1.37.2-20.3", coreManifest(k8s1372, "16.1-76.43"))
	f.pushManifest(t, "latest", coreManifest(k8s1372, "16.1-76.43"))

	reg := RemoteRegistry{}
	tags, err := reg.ListTags(context.Background(), "oci://"+f.repo(), Access{})
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if got := strings.Join(tags, ","); got != "1.37.1-17.1,1.37.2-20.3,latest" {
		t.Fatalf("tags = %s", got)
	}

	digest, err := reg.Digest(context.Background(), f.repo(), "1.37.1-17.1", Access{})
	if err != nil || digest != d1 {
		t.Fatalf("Digest = %s, %v; want %s", digest, err, d1)
	}

	data, err := reg.ReadManifest(context.Background(), f.repo()+"@"+d1, Access{})
	if err != nil || !strings.Contains(string(data), "v1.37.1+rke2r1") {
		t.Fatalf("ReadManifest by digest: %v, %q", err, data)
	}
}

func TestListTagsUnknownRepositoryAndBadName(t *testing.T) {
	f := newFakeRegistry(t)
	if _, err := (RemoteRegistry{}).ListTags(context.Background(), f.host+"/no/such", Access{}); err == nil {
		t.Fatal("listing an unknown repository must fail")
	}
	_, err := (RemoteRegistry{}).ListTags(context.Background(), "UPPER case/not valid", Access{})
	if _, ok := err.(*InvalidSourceError); !ok {
		t.Fatalf("bad repository name: want InvalidSourceError, got %v", err)
	}
}

func TestIsReleaseTag(t *testing.T) {
	for tag, want := range map[string]bool{
		"1.37.2-20.3":       true,
		"1.37.2-20.3-e3p.1": true,
		"v1.2.3":            true,
		"0.2.1":             true,
		"latest":            false,
		"1.37":              false,
		"sha256-ff52f6d9108bc5eb3b7929e0efa059f59c61836034afa2542d8e0d72b0729cff.sig": false,
		"1.37.2-20.3.att": true, // a version by its form; refused later because it holds no manifest
		"":                false,
	} {
		if got := IsReleaseTag(tag); got != want {
			t.Errorf("IsReleaseTag(%q) = %t, want %t", tag, got, want)
		}
	}
}

func TestReadManifestWithoutManifestFile(t *testing.T) {
	f := newFakeRegistry(t)
	f.push(t, f.repo()+":9.9.9", map[string]string{"other.txt": "x"})
	_, err := (RemoteRegistry{}).ReadManifest(context.Background(), f.repo()+":9.9.9", Access{})
	if !isNotARelease(err) {
		t.Fatalf("want NotAReleaseError, got %v", err)
	}
}

func TestPullSecretAuth(t *testing.T) {
	f := newFakeRegistry(t)
	f.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("reader:s3cret"))
	f.pushManifest(t, "1.0.0", coreManifest("v1.37.1+rke2r1", "16.1"))

	if _, err := (RemoteRegistry{}).ListTags(context.Background(), f.repo(), Access{}); err == nil {
		t.Fatal("anonymous listing of a protected registry must fail")
	}
	cfg := fmt.Sprintf(`{"auths":{"%s":{"auth":"%s"}}}`, f.host, base64.StdEncoding.EncodeToString([]byte("reader:s3cret")))
	tags, err := (RemoteRegistry{}).ListTags(context.Background(), f.repo(), Access{DockerConfigJSON: []byte(cfg)})
	if err != nil || len(tags) != 1 {
		t.Fatalf("listing with the pull secret: %v, %v", tags, err)
	}
	if _, err = (RemoteRegistry{}).ListTags(context.Background(), f.repo(), Access{DockerConfigJSON: []byte("not json")}); err == nil {
		t.Fatal("a broken pull secret must fail")
	}
}

// --- manifest parsing ---

func TestParseCoreManifest(t *testing.T) {
	info, err := ParseManifest(context.Background(), []byte(coreManifest(k8s1372, "16.1-76.43-e3p.1")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Lineage != suseCore || info.CreationDate != "2026-11-18" || info.CorePlatform != nil {
		t.Errorf("metadata: %+v", info)
	}
	if info.Kubernetes.Version != k8s1372 || info.Kubernetes.Image != "registry.example/rke2-tar:1.37.2_rke2r1-8.1" {
		t.Errorf("kubernetes: %+v", info.Kubernetes)
	}
	// The registry has a port: the tag is what follows the last colon after the last slash.
	if info.OperatingSystem.Version != "16.1-76.43-e3p.1" ||
		info.OperatingSystem.Image != "registry.example:5000/os/base-os-kernel-default:16.1-76.43-e3p.1" {
		t.Errorf("operating system: %+v", info.OperatingSystem)
	}
}

func TestParseCarriedManifestFile(t *testing.T) {
	// The carried release of the lab, as served (comments, helm charts with values).
	info, err := ParseManifest(context.Background(), []byte(carriedManifest), nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Lineage != suseCore || info.Kubernetes.Version != k8s1372 || info.OperatingSystem.Version != "16.1-76.43-e3p.1" {
		t.Errorf("carried manifest: %+v", info)
	}
}

func TestParseSolutionManifest(t *testing.T) {
	var asked string
	readCore := func(_ context.Context, ref string) ([]byte, error) {
		asked = ref
		return []byte(coreManifest(k8s1372, "16.1-76.43")), nil
	}
	info, err := ParseManifest(context.Background(),
		[]byte(solutionManifest(harvesterL, "oci://registry.example/core/rke2-manifest:1.37.2-20.3")), readCore)
	if err != nil {
		t.Fatal(err)
	}
	if asked != "registry.example/core/rke2-manifest:1.37.2-20.3" {
		t.Errorf("core manifest asked for: %q", asked)
	}
	if info.Lineage != harvesterL || info.CreationDate != "2026-11-19" {
		t.Errorf("a solution's lineage is the solution's name: %+v", info)
	}
	if info.CorePlatform == nil || info.CorePlatform.Name != suseCore ||
		info.CorePlatform.Image != "oci://registry.example/core/rke2-manifest:1.37.2-20.3" {
		t.Errorf("core platform: %+v", info.CorePlatform)
	}
	if info.Kubernetes.Version != k8s1372 || info.OperatingSystem.Version != "16.1-76.43" {
		t.Errorf("components come from the core platform: %+v", info)
	}

	// The core platform cannot be read: an error that is worth a retry, not "not a release".
	_, err = ParseManifest(context.Background(), []byte(solutionManifest(harvesterL, "registry.example/core:1")),
		func(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("connection refused") })
	if err == nil || isNotARelease(err) {
		t.Errorf("unreadable core platform: %v", err)
	}
}

func TestParseRefusals(t *testing.T) {
	for title, data := range map[string]string{
		"empty":            "  \n",
		"not yaml":         "{{{{",
		"no components":    "schema: v1\nmetadata:\n  name: x\n",
		"os only (tier 1)": osOnlyManifest,
		"no name":          strings.Replace(coreManifest(k8s1372, "16.1"), "  name: suse-core\n", "", 1),
	} {
		_, err := ParseManifest(context.Background(), []byte(data), nil)
		if !isNotARelease(err) {
			t.Errorf("%s: want NotAReleaseError, got %v", title, err)
		}
	}
	_, err := ParseManifest(context.Background(), []byte(osOnlyManifest), nil)
	if err == nil || !strings.Contains(err.Error(), "operating system only") {
		t.Errorf("tier 1 reason: %v", err)
	}
}

// --- reachability filter ---

func release(objName, repository, version, lineage string) lifecyclev1alpha1.AvailableRelease {
	return lifecyclev1alpha1.AvailableRelease{
		ObjectMeta: metav1.ObjectMeta{Name: objName},
		Spec: lifecyclev1alpha1.AvailableReleaseSpec{
			Source: "s", Registry: repository, Version: version, Lineage: lineage,
			Kubernetes: lifecyclev1alpha1.AvailableKubernetes{Version: k8s1372},
		},
	}
}

func TestReachable(t *testing.T) {
	const pub, lab = "registry.suse.com/beta/uc/rke2/rke2-manifest", "lab.example:5000/e3p/rke2-manifest"
	tests := []struct {
		name     string
		current  Current
		version  string
		lineage  string
		registry string
		want     bool
		reason   string
	}{
		{"newer, same lineage", Current{Version: "1.37.1-17.1", Lineage: suseCore}, "1.37.2-20.3", suseCore, pub, true, ""},
		{"carried build is higher than the release it is built from", Current{Version: "1.37.2-20.3", Lineage: suseCore},
			"1.37.2-20.3-e3p.1", suseCore, lab, true, ""},
		{"same lineage in another repository", Current{Version: "1.37.1-17.1", Lineage: suseCore, Registry: pub},
			"1.37.2-20.3-e3p.1", suseCore, lab, true, ""},
		{"equal", Current{Version: "1.37.2-20.3", Lineage: suseCore}, "1.37.2-20.3", suseCore, pub, false, "is the current release"},
		{"downgrade", Current{Version: "1.37.2-20.3", Lineage: suseCore}, "1.37.1-17.1", suseCore, pub, false, "lower than the current release"},
		{"published is lower than its carried build", Current{Version: "1.37.2-20.3-e3p.1", Lineage: suseCore},
			"1.37.2-20.3", suseCore, pub, false, "lower than the current release"},
		{"other lineage", Current{Version: "1.37.1-17.1", Lineage: suseCore}, "1.9.0", harvesterL, pub, false, "other lineage"},
		{"other lineage even when newer", Current{Version: "1.0.0", Lineage: harvesterL}, "1.37.2-20.3", suseCore, pub, false, "other lineage"},
		{"lineage unknown, same repository name", Current{Version: "1.37.1-17.1", Registry: pub}, "1.37.2-20.3-e3p.1", suseCore, lab, true, ""},
		{"lineage unknown, other repository name", Current{Version: "1.37.1-17.1", Registry: pub}, "1.37.2-20.3", suseCore,
			"registry.suse.com/harvester/manifest", false, "lineage of the current release is unknown"},
		{"lineage and repository unknown", Current{Version: "1.37.1-17.1"}, "1.37.2-20.3", suseCore, pub, false, "lineage of the current release is unknown"},
		{"current not comparable", Current{Version: "banana", Lineage: suseCore}, "1.37.2-20.3", suseCore, pub, false, "cannot be compared"},
		{"candidate not comparable", Current{Version: "1.37.1-17.1", Lineage: suseCore}, "banana", suseCore, pub, false, "cannot be compared"},
		{"build suffix counts", Current{Version: "1.37.2-20.3", Lineage: suseCore}, "1.37.2-20.4", suseCore, pub, true, ""},
		{"numeric, not text, order", Current{Version: "1.37.2-9.1", Lineage: suseCore}, "1.37.2-20.3", suseCore, pub, true, ""},
	}
	for _, tc := range tests {
		got := Reachable(tc.current, release("x", tc.registry, tc.version, tc.lineage).Spec)
		if got.Reachable != tc.want || !strings.Contains(got.Reason, tc.reason) || (tc.want && got.Reason != "") {
			t.Errorf("%s: got %+v, want reachable=%t reason~%q", tc.name, got, tc.want, tc.reason)
		}
	}
}

func TestEvaluateResolvesLineageFromCatalog(t *testing.T) {
	const pub, lab = "registry.suse.com/beta/uc/rke2/rke2-manifest", "lab.example:5000/e3p/rke2-manifest"
	releases := []lifecyclev1alpha1.AvailableRelease{
		release("pub-1.37.1-17.1", pub, "1.37.1-17.1", suseCore),
		release("pub-1.37.2-20.3", pub, "1.37.2-20.3", suseCore),
		release("lab-1.37.2-20.3-e3p.1", lab, "1.37.2-20.3-e3p.1", suseCore),
		release("hv-1.37.9", "registry.example/harvester/manifest", "1.37.9", harvesterL),
		release("mirror-1.37.1-17.1", "mirror.example/x/rke2-manifest", "1.37.1-17.1", "other-name"),
	}
	// The cluster's release is in the catalog twice; the entry of its own repository names the lineage.
	got := Evaluate(Current{Version: "1.37.1-17.1", Registry: "oci://" + pub}, releases)
	order := make([]string, 0, len(got))
	var reachable []string
	for _, c := range got {
		order = append(order, c.Release.Spec.Version)
		if c.Reach.Reachable {
			reachable = append(reachable, c.Release.Spec.Version)
		}
	}
	if strings.Join(order, " ") != "1.37.9 1.37.2-20.3-e3p.1 1.37.2-20.3 1.37.1-17.1 1.37.1-17.1" {
		t.Errorf("order (highest first): %v", order)
	}
	if strings.Join(reachable, " ") != "1.37.2-20.3-e3p.1 1.37.2-20.3" {
		t.Errorf("reachable: %v", reachable)
	}
	if l := ResolveLineage(Current{Version: "1.0.0", Registry: pub}, releases); l != "" {
		t.Errorf("a release the catalog does not hold has no known lineage, got %q", l)
	}
}

// --- controller ---

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := lifecyclev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

type harness struct {
	t      *testing.T
	client client.Client
	rec    *SourceReconciler
	clock  time.Time
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&lifecyclev1alpha1.ReleaseSource{}, &lifecyclev1alpha1.AvailableRelease{}).Build()
	h := &harness{t: t, client: c, clock: time.Date(2026, 11, 19, 18, 0, 0, 0, time.UTC)}
	h.rec = &SourceReconciler{Client: c, Scheme: s, Registry: RemoteRegistry{}, Namespace: "catalog-system",
		Now: func() time.Time { return h.clock }}
	return h
}

func (h *harness) reconcile(source string) ctrl.Result {
	h.t.Helper()
	res, err := h.rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: source}})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (h *harness) source(srcName string) *lifecyclev1alpha1.ReleaseSource {
	h.t.Helper()
	src := &lifecyclev1alpha1.ReleaseSource{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Name: srcName}, src); err != nil {
		h.t.Fatal(err)
	}
	return src
}

func (h *harness) releases() map[string]lifecyclev1alpha1.AvailableRelease {
	h.t.Helper()
	list := &lifecyclev1alpha1.AvailableReleaseList{}
	if err := h.client.List(context.Background(), list); err != nil {
		h.t.Fatal(err)
	}
	out := map[string]lifecyclev1alpha1.AvailableRelease{}
	for _, r := range list.Items {
		out[r.Name] = r
	}
	return out
}

func ready(src *lifecyclev1alpha1.ReleaseSource) *metav1.Condition {
	return apimeta.FindStatusCondition(src.Status.Conditions, lifecyclev1alpha1.ConditionSourceReady)
}

func newSource(srcName, repo string) *lifecyclev1alpha1.ReleaseSource {
	return &lifecyclev1alpha1.ReleaseSource{
		ObjectMeta: metav1.ObjectMeta{Name: srcName, UID: types.UID("uid-" + srcName)},
		Spec:       lifecyclev1alpha1.ReleaseSourceSpec{Registry: repo},
	}
}

func TestReconcilePublishesReleases(t *testing.T) {
	f := newFakeRegistry(t)
	d1 := f.pushManifest(t, "1.37.1-17.1", coreManifest("v1.37.1+rke2r1", "16.1-70.1"))
	f.pushManifest(t, "1.37.2-20.3", coreManifest(k8s1372, "16.1-76.43"))
	f.pushManifest(t, "latest", coreManifest(k8s1372, "16.1-76.43"))
	f.pushManifest(t, "1.0.0", osOnlyManifest)
	f.push(t, f.repo()+":2.0.0", map[string]string{"README": "not a release"})

	h := newHarness(t, newSource("core", "oci://"+f.repo()))
	res := h.reconcile("core")
	if res.RequeueAfter != DefaultInterval {
		t.Errorf("requeue after %s, want the default interval", res.RequeueAfter)
	}

	rels := h.releases()
	if len(rels) != 2 {
		t.Fatalf("published %d releases, want 2: %v", len(rels), rels)
	}
	r := rels["core-1.37.1-17.1"]
	want := lifecyclev1alpha1.AvailableReleaseSpec{
		Source: "core", Registry: f.repo(), Version: "1.37.1-17.1", Digest: d1, Manifest: f.repo() + "@" + d1,
		Lineage: suseCore, CreationDate: "2026-11-18",
		OperatingSystem: lifecyclev1alpha1.AvailableOperatingSystem{
			Image: "registry.example:5000/os/base-os-kernel-default:16.1-70.1", Version: "16.1-70.1"},
		Kubernetes: lifecyclev1alpha1.AvailableKubernetes{Version: "v1.37.1+rke2r1", Image: "registry.example/rke2-tar:1.37.2_rke2r1-8.1"},
	}
	if r.Spec != want {
		t.Errorf("spec:\n got %+v\nwant %+v", r.Spec, want)
	}
	if r.Labels[lifecyclev1alpha1.CatalogSourceLabel] != "core" || r.Labels[lifecyclev1alpha1.CatalogLineageLabel] != suseCore {
		t.Errorf("labels: %v", r.Labels)
	}
	if len(r.OwnerReferences) != 1 || r.OwnerReferences[0].Kind != "ReleaseSource" || r.OwnerReferences[0].Name != "core" {
		t.Errorf("owner: %v", r.OwnerReferences)
	}
	if r.Status.Stale || r.Status.DiscoveredAt == nil || !r.Status.DiscoveredAt.Time.Equal(h.clock) {
		t.Errorf("status: %+v", r.Status)
	}

	src := h.source("core")
	if c := ready(src); c == nil || c.Status != metav1.ConditionTrue || c.Reason != lifecyclev1alpha1.SourceSynced {
		t.Errorf("condition: %+v", c)
	}
	if src.Status.Stale || src.Status.Releases != 2 || src.Status.LastSuccessfulSyncTime == nil {
		t.Errorf("source status: %+v", src.Status)
	}
	reasons := map[string]string{}
	for _, s := range src.Status.Skipped {
		reasons[s.Tag] = s.Reason
	}
	if reasons["latest"] != "not a release version" || !strings.Contains(reasons["1.0.0"], "operating system only") ||
		!strings.Contains(reasons["2.0.0"], "no release_manifest.yaml") || len(reasons) != 3 {
		t.Errorf("skipped: %v", reasons)
	}
}

func TestReconcileNewReleaseAppearsAndRemovedOneGoes(t *testing.T) {
	f := newFakeRegistry(t)
	f.pushManifest(t, "1.37.1-17.1", coreManifest("v1.37.1+rke2r1", "16.1-70.1"))
	h := newHarness(t, newSource("core", f.repo()))
	h.reconcile("core")
	first := h.releases()["core-1.37.1-17.1"]

	// A new release is pushed; nobody touches the cluster.
	h.clock = h.clock.Add(5 * time.Minute)
	f.pushManifest(t, "1.37.2-20.3", coreManifest(k8s1372, "16.1-76.43"))
	h.reconcile("core")
	rels := h.releases()
	if len(rels) != 2 || rels["core-1.37.2-20.3"].Spec.Kubernetes.Version != k8s1372 {
		t.Fatalf("new release not published: %v", rels)
	}
	// An unchanged release is not rewritten.
	if again := rels["core-1.37.1-17.1"]; !again.Status.DiscoveredAt.Equal(first.Status.DiscoveredAt) ||
		again.ResourceVersion != first.ResourceVersion {
		t.Errorf("unchanged release was rewritten: %+v", again)
	}

	// A tag moved to other content: the entry follows the registry.
	h.clock = h.clock.Add(5 * time.Minute)
	d := f.pushManifest(t, "1.37.2-20.3", coreManifest("v1.37.3+rke2r1", "16.1-76.43"))
	h.reconcile("core")
	if moved := h.releases()["core-1.37.2-20.3"]; moved.Spec.Digest != d || moved.Spec.Kubernetes.Version != "v1.37.3+rke2r1" ||
		!moved.Status.DiscoveredAt.Time.Equal(h.clock) {
		t.Errorf("moved tag: %+v", moved)
	}

	// A tag is removed from the registry: the entry goes, the other stays.
	gone, err := name.ParseReference(f.repo() + ":1.37.1-17.1")
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Delete(gone); err != nil {
		t.Fatal(err)
	}
	h.reconcile("core")
	rels = h.releases()
	if _, still := rels["core-1.37.1-17.1"]; still || len(rels) != 1 {
		t.Errorf("removed tag still published: %v", rels)
	}
	if h.source("core").Status.Releases != 1 {
		t.Errorf("release count: %d", h.source("core").Status.Releases)
	}
}

func TestReconcileUnreachableRegistryKeepsLastKnownListStale(t *testing.T) {
	f := newFakeRegistry(t)
	f.pushManifest(t, "1.37.1-17.1", coreManifest("v1.37.1+rke2r1", "16.1-70.1"))
	f.pushManifest(t, "1.37.2-20.3", coreManifest(k8s1372, "16.1-76.43"))
	h := newHarness(t, newSource("core", f.repo()))
	h.reconcile("core")
	okAt := h.clock

	for title, breakIt := range map[string]func(){
		"answers 503":        func() { f.down = true },
		"connection refused": func() { f.server.Close() },
	} {
		h.clock = h.clock.Add(5 * time.Minute)
		breakIt()
		res := h.reconcile("core")
		if res.RequeueAfter != DefaultInterval {
			t.Errorf("%s: the source must be queried again, requeue %s", title, res.RequeueAfter)
		}
		rels := h.releases()
		if len(rels) != 2 {
			t.Fatalf("%s: last known list not kept: %v", title, rels)
		}
		for n, r := range rels {
			if !r.Status.Stale {
				t.Errorf("%s: %s not marked stale", title, n)
			}
		}
		src := h.source("core")
		c := ready(src)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != lifecyclev1alpha1.SourceUnreachable || c.Message == "" {
			t.Errorf("%s: condition %+v", title, c)
		}
		if !src.Status.Stale || src.Status.Releases != 2 || !src.Status.LastSuccessfulSyncTime.Time.Equal(okAt) ||
			!src.Status.LastSyncTime.Time.Equal(h.clock) {
			t.Errorf("%s: source status %+v", title, src.Status)
		}
	}
}

func TestReconcileRegistryComesBack(t *testing.T) {
	f := newFakeRegistry(t)
	f.pushManifest(t, "1.37.1-17.1", coreManifest("v1.37.1+rke2r1", "16.1-70.1"))
	h := newHarness(t, newSource("core", f.repo()))
	h.reconcile("core")
	f.down = true
	h.reconcile("core")
	if !h.releases()["core-1.37.1-17.1"].Status.Stale {
		t.Fatal("not stale while down")
	}
	f.down = false
	h.reconcile("core")
	if h.releases()["core-1.37.1-17.1"].Status.Stale || h.source("core").Status.Stale ||
		ready(h.source("core")).Status != metav1.ConditionTrue {
		t.Error("stale mark not cleared when the registry answers again")
	}
}

// partialRegistry lists tags but fails to read one of them.
type partialRegistry struct {
	RemoteRegistry
	failTag string
}

func (p partialRegistry) Digest(ctx context.Context, repository, tag string, access Access) (string, error) {
	if tag == p.failTag {
		return "", fmt.Errorf("reading digest of %s:%s: timeout", repository, tag)
	}
	return p.RemoteRegistry.Digest(ctx, repository, tag, access)
}

func TestReconcilePartialFailureKeepsKnownEntryStale(t *testing.T) {
	f := newFakeRegistry(t)
	f.pushManifest(t, "1.37.1-17.1", coreManifest("v1.37.1+rke2r1", "16.1-70.1"))
	f.pushManifest(t, "1.37.2-20.3", coreManifest(k8s1372, "16.1-76.43"))
	h := newHarness(t, newSource("core", f.repo()))
	h.reconcile("core")

	f.pushManifest(t, "1.37.3-1.1", coreManifest("v1.37.3+rke2r1", "16.1-80.1"))
	h.rec.Registry = partialRegistry{failTag: "1.37.2-20.3"}
	h.reconcile("core")

	rels := h.releases()
	if len(rels) != 3 || !rels["core-1.37.2-20.3"].Status.Stale || rels["core-1.37.1-17.1"].Status.Stale ||
		rels["core-1.37.3-1.1"].Status.Stale {
		t.Errorf("releases: %+v", rels)
	}
	src := h.source("core")
	if c := ready(src); c.Status != metav1.ConditionFalse || c.Reason != lifecyclev1alpha1.SourcePartiallySynced {
		t.Errorf("condition: %+v", c)
	}
	if src.Status.Stale {
		t.Error("the source itself answered: it is not stale as a whole")
	}
}

func TestReconcilePullSecretAndInvalidSource(t *testing.T) {
	f := newFakeRegistry(t)
	cred := base64.StdEncoding.EncodeToString([]byte("reader:s3cret"))
	f.auth = "Basic " + cred
	f.pushManifest(t, "1.37.1-17.1", coreManifest("v1.37.1+rke2r1", "16.1-70.1"))

	src := newSource("core", f.repo())
	src.Spec.PullSecretRef = &corev1.LocalObjectReference{Name: "pull"}
	src.Spec.Interval = &metav1.Duration{Duration: time.Second}
	h := newHarness(t, src)

	// The secret does not exist yet.
	res := h.reconcile("core")
	if c := ready(h.source("core")); c.Reason != lifecyclev1alpha1.SourceInvalid || !h.source("core").Status.Stale {
		t.Errorf("missing pull secret: %+v", c)
	}
	if res.RequeueAfter != MinInterval {
		t.Errorf("interval below the minimum must be raised to it, got %s", res.RequeueAfter)
	}

	// A secret of that name in another namespace is not used.
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pull", Namespace: "elsewhere"},
		Data: map[string][]byte{corev1.DockerConfigJsonKey: fmt.Appendf(nil, `{"auths":{"%s":{"auth":"%s"}}}`, f.host, cred)}}
	if err := h.client.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	h.reconcile("core")
	if len(h.releases()) != 0 {
		t.Fatal("a secret from another namespace was used")
	}

	own := other.DeepCopy()
	own.Namespace, own.ResourceVersion = "catalog-system", ""
	if err := h.client.Create(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	h.reconcile("core")
	if len(h.releases()) != 1 || ready(h.source("core")).Status != metav1.ConditionTrue {
		t.Errorf("with the pull secret: %v, %+v", h.releases(), ready(h.source("core")))
	}
}

func TestReconcileTwoSourcesDoNotTouchEachOther(t *testing.T) {
	f := newFakeRegistry(t)
	f.pushManifest(t, "1.37.2-20.3", coreManifest(k8s1372, "16.1-76.43"))
	other := f.host + "/lab/rke2-manifest"
	f.push(t, other+":1.37.2-20.3-e3p.1", map[string]string{ManifestPath: carriedManifest})

	h := newHarness(t, newSource("published", f.repo()), newSource("carried", other))
	h.reconcile("published")
	h.reconcile("carried")
	if h.source("published").Status.Releases != 1 || h.source("carried").Status.Releases != 1 {
		t.Fatal("each source counts its own releases")
	}
	rels := h.releases()
	if len(rels) != 2 || rels["carried-1.37.2-20.3-e3p.1"].Spec.Registry != other ||
		rels["published-1.37.2-20.3"].Spec.Registry != f.repo() {
		t.Fatalf("releases: %v", rels)
	}
	// Reconciling one source again leaves the other's entries alone.
	h.reconcile("published")
	if len(h.releases()) != 2 {
		t.Error("a source removed another source's release")
	}
}

func TestReconcileSourceGone(t *testing.T) {
	h := newHarness(t)
	_, err := h.rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "none"}})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestReleaseName(t *testing.T) {
	for in, want := range map[string]string{
		"1.37.2-20.3-e3p.1": "src-1.37.2-20.3-e3p.1",
		"v1.2.3+Build_7":    "src-v1.2.3-build-7",
	} {
		if got := ReleaseName("src", in); got != want {
			t.Errorf("ReleaseName(%q) = %q, want %q", in, got, want)
		}
	}
}
