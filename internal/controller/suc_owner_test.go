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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1 "github.com/k3s-io/helm-controller/pkg/apis/helm.cattle.io/v1"
	"github.com/suse/elemental-lifecycle-manager/internal/helm"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade/reconcilers"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade/reconcilers/testutil"
	"github.com/suse/elemental/v3/pkg/manifest/api"
	"github.com/suse/elemental/v3/pkg/manifest/api/core"
	"github.com/suse/elemental/v3/pkg/manifest/resolver"
)

var _ = Describe("System Upgrade Controller ownership", func() {
	var (
		sucCtx        context.Context
		reconciler    *ReleaseReconciler
		helmClient    *testutil.MockHelmClient
		manifest      *resolver.ResolvedManifest
		runtimeCharts map[string]upgrade.RuntimeHelmChartConfig
		created       []client.Object
	)

	create := func(obj client.Object) {
		GinkgoHelper()
		Expect(k8sClient.Create(sucCtx, obj)).To(Succeed())
		created = append(created, obj)
	}

	ensureNamespace := func(name string) {
		GinkgoHelper()
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(sucCtx, ns))).To(Succeed())
	}

	clusterAgent := func() *appsv1.Deployment {
		labels := map[string]string{"app": rancherClusterAgent}
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: rancherClusterAgent, Namespace: rancherNamespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "cluster-register", Image: "rancher/rancher-agent"}},
					},
				},
			},
		}
	}

	chartNames := func(h *api.Helm) []string {
		names := make([]string, 0, len(h.Charts))
		for _, c := range h.Charts {
			names = append(names, c.Chart)
		}
		return names
	}

	BeforeEach(func() {
		sucCtx = context.Background()
		created = nil

		ensureNamespace(rancherNamespace)
		ensureNamespace(reconcilers.HelmChartNamespace)

		helmClient = testutil.NewMockHelmClient()
		reconciler = &ReleaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), HelmClient: helmClient}

		manifest = &resolver.ResolvedManifest{
			CorePlatform: &core.ReleaseManifest{
				Components: core.Components{
					Helm: &api.Helm{Charts: []*api.HelmChart{
						{Chart: "cert-manager", Version: "v1.20.3"},
						{Chart: sucChartName, Version: "109.0.2"},
						{Chart: rancherName, Version: "2.14.0"},
					}},
				},
			},
		}
		runtimeCharts = map[string]upgrade.RuntimeHelmChartConfig{sucChartName: {}, "cert-manager": {}}
	})

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(sucCtx, obj))).To(Succeed())
		}
	})

	It("keeps the chart when there is no trace of Rancher", func() {
		Expect(reconciler.handleSUCOwner(sucCtx, manifest, runtimeCharts)).To(Succeed())

		Expect(chartNames(manifest.CorePlatform.Components.Helm)).To(ContainElement(sucChartName))
		Expect(runtimeCharts).To(HaveKey(sucChartName))
	})

	It("drops the chart on a cluster managed by a Rancher that runs elsewhere", func() {
		create(clusterAgent())

		Expect(reconciler.handleSUCOwner(sucCtx, manifest, runtimeCharts)).To(Succeed())

		Expect(chartNames(manifest.CorePlatform.Components.Helm)).To(ConsistOf("cert-manager", rancherName))
		Expect(runtimeCharts).NotTo(HaveKey(sucChartName))
		Expect(runtimeCharts).To(HaveKey("cert-manager"))
	})

	It("drops the chart when Rancher is installed as a Helm release", func() {
		helmClient.RetrieveNamespacedReleaseFn = func(namespace, name string) (*helm.ReleaseInfo, error) {
			if namespace == rancherNamespace && name == rancherName {
				return &helm.ReleaseInfo{}, nil
			}
			return nil, helm.ErrReleaseNotFound
		}

		Expect(reconciler.handleSUCOwner(sucCtx, manifest, runtimeCharts)).To(Succeed())

		Expect(chartNames(manifest.CorePlatform.Components.Helm)).To(ConsistOf("cert-manager", rancherName))
		Expect(runtimeCharts).NotTo(HaveKey(sucChartName))
	})

	It("keeps the chart when Rancher is deployed outside of its supported namespace", func() {
		create(&helmv1.HelmChart{
			ObjectMeta: metav1.ObjectMeta{Name: rancherName, Namespace: reconcilers.HelmChartNamespace},
			Spec:       helmv1.HelmChartSpec{Chart: rancherName, TargetNamespace: "somewhere-else"},
		})

		Expect(reconciler.handleSUCOwner(sucCtx, manifest, runtimeCharts)).To(Succeed())

		Expect(chartNames(manifest.CorePlatform.Components.Helm)).To(ContainElement(sucChartName))
		Expect(runtimeCharts).To(HaveKey(sucChartName))
	})

	It("copes with a release manifest without Helm charts", func() {
		create(clusterAgent())
		manifest.CorePlatform.Components.Helm = nil

		Expect(reconciler.handleSUCOwner(sucCtx, manifest, runtimeCharts)).To(Succeed())
		Expect(runtimeCharts).NotTo(HaveKey(sucChartName))
	})
})
