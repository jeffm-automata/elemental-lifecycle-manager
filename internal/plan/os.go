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
	osUpgradeShell         = "/bin/sh"

	// osJobDeadline is the time a single node is given for its OS upgrade job: the pull of the OS image,
	// the drain, the transaction, the reboot and the pod that confirms the result once the node is back.
	// Apart from the drain these steps have been measured to take up to 15 minutes on a node whose
	// cluster was degraded by the upgrade itself, so the job gets the full drain timeout plus twice that.
	osJobDeadline = drainTimeout + 30*time.Minute
	// osPrepareScript is the command of the prepare container of OS plans. The container has
	// nothing to do, it only exists so that the OS image is pulled before the node is drained.
	osPrepareScript = `echo "OS image is available on the node"`
)

//go:embed templates/os-upgrade.sh.tpl
var osUpgradeScriptTpl string

// IsOS reports whether the given SUC Plan name is the name of an operating system upgrade plan.
func IsOS(planName string) bool {
	return strings.HasPrefix(planName, osControlPlaneBaseName+"-") || strings.HasPrefix(planName, osWorkerBaseName+"-")
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
