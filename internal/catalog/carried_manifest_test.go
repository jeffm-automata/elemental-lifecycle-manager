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

// carriedManifest is the carried release 1.37.2-20.3-e3p.1 of the Elemental 3 parity lab
// (registry and chart server replaced by example names).
const carriedManifest = `# CARRIED RELEASE 1.37.2-20.3-e3p.1 (Elemental 3 parity project; not a SUSE release).
# The published 1.37.2-20.3 (registry.suse.com/beta/uc/rke2/rke2-manifest@sha256:ff52f6d9108b...) with the changes
# marked CARRIED below. Every change, its reason and its source are listed in ../CARRY.md.
# lab.example:5000 is the registry and http://lab.example:8080/charts the chart repository of the lab instance that serves the release
# (build/host-build.sh fills them in; the proof used lab.example:5000 and http://lab.example:8080/charts).
schema: v1
metadata:
  name: suse-core
  creationDate: '2026-11-18'
  elemental:
    version: '3.1'
    # CARRIED 1: elemental3 built from jeffm-automata/elemental agent/CARRY-e3p.1 (published: beta/uc/elemental:3.1.0-7.25).
    image: lab.example:5000/e3p/elemental:3.1.0-e3p.1
components:
  operatingSystem:
    image:
      # CARRIED 2: published base-os-kernel-default:16.1-76.43 with /usr/bin/elemental3ctl replaced by the carried build.
      base: lab.example:5000/e3p/base-os-kernel-default:16.1-76.43-e3p.1
      iso: registry.suse.com/beta/uc/base-os-kernel-default-iso:16.1-76.81
  kubernetes:
    version: v1.37.2+rke2r1
    image: registry.suse.com/beta/uc/rke2-tar:1.37.2_rke2r1-8.1
  helm:
    charts:
    - name: MetalLB
      chart: metallb
      version: 0.15.2
      namespace: metallb-system
      repository: metallb
    - name: Endpoint Copier Operator
      chart: endpoint-copier-operator
      version: 0.3.0
      namespace: endpoint-copier-operator
      repository: suse-edge
    # CARRIED 3: Lifecycle Manager chart and image built from jeffm-automata/elemental-lifecycle-manager
    # agent/CARRY-e3p.1 (published: chart 0.2.0, whose default image does not exist). The image is also named in
    # values, because a cluster installed from the published release carries a build-time override of
    # image.repository in its HelmChart, which would otherwise survive the chart upgrade.
    - name: Elemental Lifecycle Manager
      chart: elemental-lifecycle-manager
      version: 0.2.1-e3p.1
      namespace: elemental-system
      repository: elemental-charts
      values:
        image:
          repository: lab.example:5000/e3p/elemental-lifecycle-manager
          tag: 0.2.1-e3p.1
      dependsOn:
      - name: cert-manager
        type: helm
      - name: elemental-lifecycle-manager-crds
        type: helm
      - name: system-upgrade-controller
        type: helm
    - name: Cert Manager
      chart: cert-manager
      version: v1.20.3
      namespace: cert-manager
      repository: jetstack
      values:
        crds:
          enabled: true
    - name: Elemental Lifecycle Manager CRDs
      chart: elemental-lifecycle-manager-crds
      version: 0.2.1-e3p.1
      namespace: elemental-system
      repository: elemental-charts
    # CARRIED 4: upgrade controller that can drain on Kubernetes 1.37 (published: chart 109.0.2, which cannot).
    #  - chart 110.0.0 from charts.rancher.io, UNCHANGED, mirrored into the lab chart repository;
    #  - kubectl image: the Kubernetes project's own kubectl image at THIS RELEASE'S Kubernetes version
    #    (registry.k8s.io/kubectl, mirrored unchanged) instead of rancher/kuberlr-kubectl, whose wrapper refuses to
    #    run when it has no kubectl within one minor of the server (the cause of the failure). The tag must equal
    #    components.kubernetes.version without the +rke2 suffix; build/host-build.sh refuses to build otherwise;
    #  - priorityUpgrade, so that on an existing cluster the controller is replaced before the first drain;
    #  - both images are served from the lab registry (systemDefaultRegistry is prepended to both repositories).
    - name: System Upgrade Controller
      chart: system-upgrade-controller
      version: 110.0.0
      namespace: cattle-system
      repository: elemental-charts
      priorityUpgrade: true
      values:
        global:
          cattle:
            systemDefaultRegistry: lab.example:5000
        kubectl:
          image:
            repository: kubectl
            tag: v1.37.2
    repositories:
    - name: metallb
      url: https://metallb.github.io/metallb
    - name: suse-edge
      url: https://suse-edge.github.io/charts
    - name: jetstack
      url: https://charts.jetstack.io
    # CARRIED 5: lab chart repository (published: oci://registry.suse.com/beta/uc/charts).
    - name: elemental-charts
      url: http://lab.example:8080/charts
    - name: rancher-charts
      url: https://charts.rancher.io/
`
