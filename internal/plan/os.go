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

package plan

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	upgradecattlev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	osControlPlaneBaseName = "elemental-os-control-plane"
	osWorkerBaseName       = "elemental-os-worker"
	osPrePullBaseName      = "elemental-os-prepull"
	osUpgradeShell         = "/bin/sh"

	// osJobDeadline is the time a single node is given for its OS upgrade job: the pull of the OS image,
	// the drain, the transaction, the reboot and the pod that confirms the result once the node is back.
	// Apart from the drain these steps have been measured to take up to 15 minutes on a node whose
	// cluster was degraded by the upgrade itself, so the job gets the full drain timeout plus twice that.
	osJobDeadline = drainTimeout + 30*time.Minute
	// osPrepareScript is the command of the prepare container of OS plans. The container has
	// nothing to do, it only exists so that the OS image is pulled before the node is drained.
	osPrepareScript = `echo "OS image is available on the node"`

	// kubernetesPrePullScript is the command of the container that only exists so that the image of the
	// Kubernetes upgrade is pulled in advance.
	kubernetesPrePullScript = `echo "Kubernetes upgrade image is available on the node"`

	// osPrePullConcurrency is the number of nodes that pull the OS image in advance at the same time.
	// It bounds the load on the registry that serves the image.
	osPrePullConcurrency = 5
	// osPrePullJobDeadline is the time a single node is given to pull the OS image in advance.
	osPrePullJobDeadline = 30 * time.Minute
)

//go:embed templates/os-upgrade.sh.tpl
var osUpgradeScriptTpl string

// IsOS reports whether the given SUC Plan name is the name of an operating system upgrade plan.
func IsOS(planName string) bool {
	return strings.HasPrefix(planName, osControlPlaneBaseName+"-") || strings.HasPrefix(planName, osWorkerBaseName+"-") ||
		strings.HasPrefix(planName, osPrePullBaseName+"-")
}

// osPrePullName returns the full plan name for the given version.
func osPrePullName(version string) string {
	return fmt.Sprintf("%s-%s", osPrePullBaseName, lifecyclev1alpha1.SanitizeVersion(version))
}

// OSPrePull builds a SUC Plan that makes every node of the cluster pull the OS image, without upgrading anything.
//
// The upgrade plans pull the image in the prepare step of each node, when it is the turn of that node. As nodes
// are upgraded one after the other, the pulls add up in the duration of the upgrade. This plan neither cordons nor
// drains a node and runs on several nodes at once, so that the image is already present on a node when its turn comes.
// It is an optimisation only: the upgrade plans do not wait for it and still pull the image themselves if needed.
//
// A non-empty kubernetesImage is pulled as well: it is the image of the Kubernetes upgrade that follows the OS upgrade,
// which a node otherwise pulls after it has been cordoned and drained.
func OSPrePull(releaseName, osImage, osVersion, releaseVersion, kubernetesImage string) *upgradecattlev1.Plan {
	p := basePlan(osPrePullName(osVersion), false, osPrePullJobDeadline)
	// Without a drain the base plan cordons the node, which a node that only pulls an image must not be.
	p.Spec.Cordon = false
	p.Labels = map[string]string{
		lifecyclev1alpha1.ReleaseNameLabel:    releaseName,
		lifecyclev1alpha1.ReleaseVersionLabel: lifecyclev1alpha1.SanitizeVersion(releaseVersion),
	}
	p.Spec.Version = osVersion
	p.Spec.Concurrency = osPrePullConcurrency
	// SUC applies a plan without a node selector to no node at all. Every node has a hostname label.
	p.Spec.NodeSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      hostnameLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	}
	if kubernetesImage != "" {
		p.Spec.Prepare = &upgradecattlev1.ContainerSpec{
			Image:   kubernetesImage,
			Command: []string{osUpgradeShell, "-c"},
			Args:    []string{kubernetesPrePullScript},
		}
	}
	p.Spec.Upgrade = osPrepareContainer(osImage)
	return p
}

// osControlPlaneName returns the full plan name for the given version.
func osControlPlaneName(version string) string {
	return fmt.Sprintf("%s-%s", osControlPlaneBaseName, lifecyclev1alpha1.SanitizeVersion(version))
}

// osWorkerName returns the full plan name for the given version.
func osWorkerName(version string) string {
	return fmt.Sprintf("%s-%s", osWorkerBaseName, lifecyclev1alpha1.SanitizeVersion(version))
}

// OSControlPlane builds a SUC Plan for OS upgrades on control plane nodes.
func OSControlPlane(releaseName, osImage, osVersion, releaseVersion string, drain bool) (*upgradecattlev1.Plan, error) {
	script, err := parseUpgradeScript(osImage)
	if err != nil {
		return nil, fmt.Errorf("parsing OS upgrade script: %w", err)
	}

	p := basePlan(osControlPlaneName(osVersion), drain, osJobDeadline)
	p.Labels = map[string]string{
		lifecyclev1alpha1.ReleaseNameLabel:    releaseName,
		lifecyclev1alpha1.ReleaseVersionLabel: lifecyclev1alpha1.SanitizeVersion(releaseVersion),
	}
	p.Spec.Version = osVersion
	p.Spec.Concurrency = 1
	p.Spec.NodeSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      ControlPlaneLabel,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	}
	p.Spec.Prepare = osPrepareContainer(osImage)
	p.Spec.Upgrade = &upgradecattlev1.ContainerSpec{
		Image:   osImage,
		Command: []string{osUpgradeShell, "-c"},
		Args:    []string{script},
	}
	return p, nil
}

// OSWorker builds a SUC Plan for OS upgrades on worker nodes.
// The concurrency is the number of worker nodes that are upgraded at the same time.
func OSWorker(releaseName, osImage, osVersion, releaseVersion string, drain bool, concurrency int64) (*upgradecattlev1.Plan, error) {
	script, err := parseUpgradeScript(osImage)
	if err != nil {
		return nil, fmt.Errorf("parsing OS upgrade script: %w", err)
	}

	p := basePlan(osWorkerName(osVersion), drain, osJobDeadline)
	p.Labels = map[string]string{
		lifecyclev1alpha1.ReleaseNameLabel:    releaseName,
		lifecyclev1alpha1.ReleaseVersionLabel: lifecyclev1alpha1.SanitizeVersion(releaseVersion),
	}
	p.Spec.Version = osVersion
	p.Spec.Concurrency = workerConcurrency(concurrency)
	p.Spec.NodeSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      ControlPlaneLabel,
				Operator: metav1.LabelSelectorOpDoesNotExist,
			},
		},
	}
	p.Spec.Prepare = osPrepareContainer(osImage)
	p.Spec.Upgrade = &upgradecattlev1.ContainerSpec{
		Image:   osImage,
		Command: []string{osUpgradeShell, "-c"},
		Args:    []string{script},
	}
	return p, nil
}

// osPrepareContainer returns the prepare step of an OS plan.
//
// SUC runs the prepare container of a Plan before it cordons and drains the node, while the image
// of the upgrade container is only pulled once the drain has finished. Running a no-op prepare container
// from the OS image makes the node pull it while it still serves its workloads, and the upgrade
// container then starts from the image that is already present.
func osPrepareContainer(osImage string) *upgradecattlev1.ContainerSpec {
	return &upgradecattlev1.ContainerSpec{
		Image:   osImage,
		Command: []string{osUpgradeShell, "-c"},
		Args:    []string{osPrepareScript},
	}
}

func parseUpgradeScript(osImage string) (string, error) {
	ref, err := name.NewTag(osImage, name.WeakValidation)
	if err != nil {
		return "", fmt.Errorf("parsing OS image %q: %w", osImage, err)
	}

	tmpl, err := template.New("os-upgrade").Parse(osUpgradeScriptTpl)
	if err != nil {
		return "", fmt.Errorf("allocating template for OS upgrade script: %w", err)
	}

	// By default OS upgrade will be done by using the locally available OS image.
	// This functionality gives us the option to override that default
	// and pull the image from its remote instead.
	fetchRemote := func() bool {
		value, err := strconv.ParseBool(os.Getenv("E3CTL_FETCH_REMOTE"))
		return err == nil && value
	}

	data := struct {
		FetchRemote    bool
		OSImage        string
		OSImageRepo    string
		OSImageVersion string
	}{
		FetchRemote: fetchRemote(),
		// The release marker of a freshly installed system names the OS image exactly
		// as the release manifest does, so the reference is passed on unmodified.
		OSImage:        osImage,
		OSImageRepo:    ref.Context().Name(),
		OSImageVersion: ref.TagStr(),
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("applying template for OS upgrade script: %w", err)
	}

	return out.String(), nil
}
