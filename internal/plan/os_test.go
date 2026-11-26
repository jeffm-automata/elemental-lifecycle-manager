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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	upgradecattlev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

var _ = Describe("OS plan tests", func() {
	const (
		releaseName           = "test-release"
		releaseVersion        = "0.6.0"
		osImage               = "registry.example.com/elemental-os:1.2.3"
		osVersion             = "1.2.3"
		drain                 = true
		expectedUpgradeScript = `set -e

HOST="${HOST:-/host}"
DEPLOYMENT="${DEPLOYMENT:-$HOST/etc/elemental/deployment.yaml}"
RELEASE_MARKER="${RELEASE_MARKER:-$HOST/etc/elemental/release.yaml}"
OS_IMAGE="registry.example.com/elemental-os:1.2.3"
OS_IMAGE_REPO="registry.example.com/elemental-os"
OS_VERSION="1.2.3"
INCOMING="$OS_IMAGE_REPO:$OS_VERSION"
CURRENT=$(grep -F "uri: oci://$OS_IMAGE_REPO" "$DEPLOYMENT" 2>/dev/null || true)
SOURCE=$(grep -F "uri: " "$DEPLOYMENT" 2>/dev/null || true)

if [ -n "$CURRENT" ]; then
	# Extract the prefix (e.g. "uri: oci://") before the OS_IMAGE_REPO,
	# so that it can be stripped in the next step.
    prefix=${CURRENT%%"$OS_IMAGE_REPO"*}
	CURRENT=${CURRENT#"$prefix"}
    if [ "$CURRENT" = "$INCOMING" ]; then
        echo "Active OS image is already at correct version $OS_VERSION. Upgrade has been performed."
        exit 0
    fi
fi

# On fresh systems, we have a sourceOS specified with raw (e.g. raw://../squashfs.img) data
# instead of from an OCI image, so the deployment does not tell which OS image the system runs.
# Images built by 'elemental3 customize' record that in a release marker, which names the
# OS image exactly as the release manifest that the image was built from does.
#
# The marker describes the installation only, so it is ignored as soon as the deployment refers
# to an OCI image. Without a marker, or with one that names any other image, we assume that the
# system has to be upgraded.
case "$SOURCE" in
    "" | *"uri: oci://"*) ;;
    *)
        INSTALLED=$(sed -n 's/^osImage:[[:space:]]*\(.*[^[:space:]]\)[[:space:]]*$/\1/p' "$RELEASE_MARKER" 2>/dev/null | head -n 1)
        if [ -n "$INSTALLED" ] && [ "$INSTALLED" = "$OS_IMAGE" ]; then
            echo "System was installed from OS image $OS_IMAGE. No upgrade needed."
            exit 0
        fi
        ;;
esac

upgrader "$INCOMING" && chroot /host reboot
`
	)

	Describe("osControlPlaneName", func() {
		It("generates correct name with sanitized version", func() {
			name := osControlPlaneName("1.2.3")
			Expect(name).To(Equal("elemental-os-control-plane-1-2-3"))
		})

		It("handles version without dots", func() {
			name := osControlPlaneName("v1")
			Expect(name).To(Equal("elemental-os-control-plane-v1"))
		})
	})

	Describe("osWorkerName", func() {
		It("generates correct name with sanitized version", func() {
			name := osWorkerName("1.2.3")
			Expect(name).To(Equal("elemental-os-worker-1-2-3"))
		})

		It("handles version without dots", func() {
			name := osWorkerName("v1")
			Expect(name).To(Equal("elemental-os-worker-v1"))
		})
	})

	Describe("OSControlPlane", Ordered, func() {
		var plan *upgradecattlev1.Plan

		BeforeEach(func() {
			var err error
			plan, err = OSControlPlane(releaseName, osImage, osVersion, releaseVersion, drain)
			Expect(err).ToNot(HaveOccurred())
		})

		It("creates a plan with correct metadata", func() {
			Expect(plan).ToNot(BeNil())
			Expect(plan.TypeMeta.Kind).To(Equal("Plan"))
			Expect(plan.TypeMeta.APIVersion).To(Equal("upgrade.cattle.io/v1"))
			Expect(plan.ObjectMeta.Name).To(Equal("elemental-os-control-plane-1-2-3"))
			Expect(plan.ObjectMeta.Namespace).To(Equal(Namespace))
		})

		It("sets release labels", func() {
			Expect(plan.Labels).To(HaveKeyWithValue(lifecyclev1alpha1.ReleaseNameLabel, releaseName))
			Expect(plan.Labels).To(HaveKeyWithValue(lifecyclev1alpha1.ReleaseVersionLabel, "0-6-0"))
		})

		It("sets correct spec version", func() {
			Expect(plan.Spec.Version).To(Equal(osVersion))
		})

		It("sets concurrency to 1", func() {
			Expect(plan.Spec.Concurrency).To(Equal(int64(1)))
		})

		It("selects control plane nodes", func() {
			Expect(plan.Spec.NodeSelector).ToNot(BeNil())
			Expect(plan.Spec.NodeSelector.MatchExpressions).To(HaveLen(1))

			expr := plan.Spec.NodeSelector.MatchExpressions[0]
			Expect(expr.Key).To(Equal("node-role.kubernetes.io/control-plane"))
			Expect(expr.Operator).To(Equal(metav1.LabelSelectorOpExists))
		})

		It("configures upgrade container", func() {
			Expect(plan.Spec.Upgrade).ToNot(BeNil())
			Expect(plan.Spec.Upgrade.Image).To(Equal(osImage))
			Expect(plan.Spec.Upgrade.Command).To(Equal([]string{osUpgradeShell, "-c"}))
			Expect(plan.Spec.Upgrade.Args).To(Equal([]string{expectedUpgradeScript}))
		})

		It("pulls the OS image before the node is drained", func() {
			Expect(plan.Spec.Prepare).ToNot(BeNil())
			Expect(plan.Spec.Prepare.Image).To(Equal(plan.Spec.Upgrade.Image))
			Expect(plan.Spec.Prepare.Command).To(Equal([]string{osUpgradeShell, "-c"}))
			Expect(plan.Spec.Prepare.Args).To(Equal([]string{osPrepareScript}))
			Expect(plan.Spec.Prepare.Args[0]).ToNot(ContainSubstring("upgrader"))
			Expect(plan.Spec.Prepare.Args[0]).ToNot(ContainSubstring("reboot"))
		})

		It("gives the upgrade job of a node enough time", func() {
			Expect(plan.Spec.JobActiveDeadlineSecs).ToNot(BeNil())
			Expect(*plan.Spec.JobActiveDeadlineSecs).To(Equal(int64(2700)))

			By("leaving room for a drain that takes its full timeout")
			timeout, err := time.ParseDuration(plan.Spec.Drain.Timeout.String())
			Expect(err).ToNot(HaveOccurred())
			Expect(time.Duration(*plan.Spec.JobActiveDeadlineSecs) * time.Second).To(BeNumerically(">", timeout))
		})

		It("enables drain with correct settings", func() {
			Expect(plan.Spec.Drain).ToNot(BeNil())
			Expect(ptr.Deref(plan.Spec.Drain.DeleteEmptydirData, false)).To(BeTrue())
			Expect(ptr.Deref(plan.Spec.Drain.IgnoreDaemonSets, false)).To(BeTrue())
			Expect(plan.Spec.Drain.Force).To(BeTrue())
			Expect(plan.Spec.Drain.Timeout.String()).To(Equal("15m"))
		})
	})

	Describe("OSPrePull", Ordered, func() {
		var plan *upgradecattlev1.Plan

		BeforeEach(func() {
			plan = OSPrePull(releaseName, osImage, osVersion, releaseVersion, "")
		})

		It("creates a plan with correct metadata", func() {
			Expect(plan).ToNot(BeNil())
			Expect(plan.TypeMeta.Kind).To(Equal("Plan"))
			Expect(plan.TypeMeta.APIVersion).To(Equal("upgrade.cattle.io/v1"))
			Expect(plan.ObjectMeta.Name).To(Equal("elemental-os-prepull-1-2-3"))
			Expect(plan.ObjectMeta.Namespace).To(Equal(Namespace))
			Expect(IsOS(plan.Name)).To(BeTrue())
		})

		It("sets release labels", func() {
			Expect(plan.Labels).To(HaveKeyWithValue(lifecyclev1alpha1.ReleaseNameLabel, releaseName))
			Expect(plan.Labels).To(HaveKeyWithValue(lifecyclev1alpha1.ReleaseVersionLabel, "0-6-0"))
		})

		It("neither cordons nor drains a node", func() {
			Expect(plan.Spec.Cordon).To(BeFalse())
			Expect(plan.Spec.Drain).To(BeNil())
		})

		It("selects every node and pulls on several nodes at the same time", func() {
			Expect(plan.Spec.NodeSelector).ToNot(BeNil())
			Expect(plan.Spec.NodeSelector.MatchExpressions).To(HaveLen(1))
			Expect(plan.Spec.NodeSelector.MatchExpressions[0].Key).To(Equal("kubernetes.io/hostname"))
			Expect(plan.Spec.NodeSelector.MatchExpressions[0].Operator).To(Equal(metav1.LabelSelectorOpExists))
			Expect(plan.Spec.Concurrency).To(Equal(int64(5)))
		})

		It("only pulls the OS image", func() {
			Expect(plan.Spec.Version).To(Equal(osVersion))
			Expect(plan.Spec.Prepare).To(BeNil())
			Expect(plan.Spec.Upgrade).ToNot(BeNil())
			Expect(plan.Spec.Upgrade.Image).To(Equal(osImage))
			Expect(plan.Spec.Upgrade.Command).To(Equal([]string{"/bin/sh", "-c"}))
			Expect(plan.Spec.Upgrade.Args).To(Equal([]string{`echo "OS image is available on the node"`}))
		})

		It("pulls the image of the Kubernetes upgrade as well when it is given", func() {
			Expect(KubernetesUpgradeImage("v1.37.2+rke2r1")).To(Equal("rancher/rke2-upgrade:v1.37.2-rke2r1"))
			plan = OSPrePull(releaseName, osImage, osVersion, releaseVersion, KubernetesUpgradeImage("v1.37.2+rke2r1"))
			Expect(plan.Spec.Prepare).ToNot(BeNil())
			Expect(plan.Spec.Prepare.Image).To(Equal("rancher/rke2-upgrade:v1.37.2-rke2r1"))
			Expect(plan.Spec.Prepare.Command).To(Equal([]string{"/bin/sh", "-c"}))
			Expect(plan.Spec.Upgrade.Image).To(Equal(osImage))
			Expect(plan.Spec.Cordon).To(BeFalse())
			Expect(plan.Spec.Drain).To(BeNil())
		})

		It("gives a node time to pull the image", func() {
			Expect(plan.Spec.JobActiveDeadlineSecs).To(Equal(ptr.To(int64((30 * time.Minute).Seconds()))))
		})
	})

	Describe("OSWorker", Ordered, func() {
		var plan *upgradecattlev1.Plan

		BeforeEach(func() {
			var err error
			plan, err = OSWorker(releaseName, osImage, osVersion, releaseVersion, drain, 1)
			Expect(err).ToNot(HaveOccurred())
		})

		It("creates a plan with correct metadata", func() {
			Expect(plan).ToNot(BeNil())
			Expect(plan.TypeMeta.Kind).To(Equal("Plan"))
			Expect(plan.TypeMeta.APIVersion).To(Equal("upgrade.cattle.io/v1"))
			Expect(plan.ObjectMeta.Name).To(Equal("elemental-os-worker-1-2-3"))
			Expect(plan.ObjectMeta.Namespace).To(Equal(Namespace))
		})

		It("sets release labels", func() {
			Expect(plan.Labels).To(HaveKeyWithValue(lifecyclev1alpha1.ReleaseNameLabel, releaseName))
			Expect(plan.Labels).To(HaveKeyWithValue(lifecyclev1alpha1.ReleaseVersionLabel, "0-6-0"))
		})

		It("sets correct spec version", func() {
			Expect(plan.Spec.Version).To(Equal(osVersion))
		})

		It("sets concurrency to 1", func() {
			Expect(plan.Spec.Concurrency).To(Equal(int64(1)))
		})

		It("selects worker nodes (not control plane)", func() {
			Expect(plan.Spec.NodeSelector).ToNot(BeNil())
			Expect(plan.Spec.NodeSelector.MatchExpressions).To(HaveLen(1))

			expr := plan.Spec.NodeSelector.MatchExpressions[0]
			Expect(expr.Key).To(Equal("node-role.kubernetes.io/control-plane"))
			Expect(expr.Operator).To(Equal(metav1.LabelSelectorOpDoesNotExist))
		})

		It("configures upgrade container", func() {
			Expect(plan.Spec.Upgrade).ToNot(BeNil())
			Expect(plan.Spec.Upgrade.Image).To(Equal(osImage))
			Expect(plan.Spec.Upgrade.Command).To(Equal([]string{osUpgradeShell, "-c"}))
			Expect(plan.Spec.Upgrade.Args).To(Equal([]string{expectedUpgradeScript}))
		})

		It("pulls the OS image before the node is drained", func() {
			Expect(plan.Spec.Prepare).ToNot(BeNil())
			Expect(plan.Spec.Prepare.Image).To(Equal(plan.Spec.Upgrade.Image))
			Expect(plan.Spec.Prepare.Command).To(Equal([]string{osUpgradeShell, "-c"}))
			Expect(plan.Spec.Prepare.Args).To(Equal([]string{osPrepareScript}))
			Expect(plan.Spec.Prepare.Args[0]).ToNot(ContainSubstring("upgrader"))
			Expect(plan.Spec.Prepare.Args[0]).ToNot(ContainSubstring("reboot"))
		})

		It("gives the upgrade job of a node enough time", func() {
			Expect(plan.Spec.JobActiveDeadlineSecs).ToNot(BeNil())
			Expect(*plan.Spec.JobActiveDeadlineSecs).To(Equal(int64(2700)))

			By("leaving room for a drain that takes its full timeout")
			timeout, err := time.ParseDuration(plan.Spec.Drain.Timeout.String())
			Expect(err).ToNot(HaveOccurred())
			Expect(time.Duration(*plan.Spec.JobActiveDeadlineSecs) * time.Second).To(BeNumerically(">", timeout))
		})

		It("enables drain with correct settings", func() {
			Expect(plan.Spec.Drain).ToNot(BeNil())
			Expect(ptr.Deref(plan.Spec.Drain.DeleteEmptydirData, false)).To(BeTrue())
			Expect(ptr.Deref(plan.Spec.Drain.IgnoreDaemonSets, false)).To(BeTrue())
			Expect(plan.Spec.Drain.Force).To(BeTrue())
			Expect(plan.Spec.Drain.Timeout.String()).To(Equal("15m"))
		})
	})

	Describe("Worker concurrency", func() {
		It("upgrades the requested number of worker nodes at the same time", func() {
			worker, err := OSWorker(releaseName, osImage, osVersion, releaseVersion, drain, 3)
			Expect(err).ToNot(HaveOccurred())
			Expect(worker.Spec.Concurrency).To(Equal(int64(3)))
		})

		It("falls back to one worker node at a time for invalid values", func() {
			for _, concurrency := range []int64{0, -2} {
				worker, err := OSWorker(releaseName, osImage, osVersion, releaseVersion, drain, concurrency)
				Expect(err).ToNot(HaveOccurred())
				Expect(worker.Spec.Concurrency).To(Equal(int64(1)))
			}
		})
	})

	Describe("Plans without drain", func() {
		It("still pull the OS image in the prepare step", func() {
			cp, err := OSControlPlane(releaseName, osImage, osVersion, releaseVersion, false)
			Expect(err).ToNot(HaveOccurred())
			Expect(cp.Spec.Drain).To(BeNil())
			Expect(cp.Spec.Prepare).ToNot(BeNil())
			Expect(cp.Spec.Prepare.Image).To(Equal(osImage))

			worker, err := OSWorker(releaseName, osImage, osVersion, releaseVersion, false, 1)
			Expect(err).ToNot(HaveOccurred())
			Expect(worker.Spec.Drain).To(BeNil())
			Expect(worker.Spec.Prepare).ToNot(BeNil())
			Expect(worker.Spec.Prepare.Image).To(Equal(osImage))
		})
	})

	Describe("Upgrade script execution", func() {
		const (
			rawDeployment = `# self-generated content, do not edit

sourceOS:
    uri: raw:///run/initramfs/live/LiveOS/squashfs.img
disks:
    - partitions: []
`
			ociDeploymentFmt = `# self-generated content, do not edit

sourceOS:
    digest: sha256:2d195da213f9ae85ff9444f224aa487e9dbd3cd8891599ae022eeb5f04a17fb2
    uri: oci://%s
disks:
    - partitions: []
`
			markerFmt = `# self-generated content, do not edit

releaseManifest: oci://registry.example.com/release-manifest:0.6.0
osImage: %s
installerImage: registry.example.com/elemental-os-iso:1.2.3
`
			noDeployment = ""
			noMarker     = ""
		)

		// runUpgradeScript runs the OS upgrade script against a fake host and reports
		// its output and the image the OS upgrade was started with, if it was started at all.
		runUpgradeScript := func(deployment, marker string) (output, upgradedTo string, rebooted bool) {
			script, err := parseUpgradeScript(osImage)
			Expect(err).ToNot(HaveOccurred())

			host := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(host, "etc", "elemental"), 0o755)).To(Succeed())
			if deployment != noDeployment {
				Expect(os.WriteFile(filepath.Join(host, "etc", "elemental", "deployment.yaml"), []byte(deployment), 0o444)).To(Succeed())
			}
			if marker != noMarker {
				Expect(os.WriteFile(filepath.Join(host, "etc", "elemental", "release.yaml"), []byte(marker), 0o444)).To(Succeed())
			}

			// The script ends in 'upgrader <image> && chroot /host reboot'; both commands are replaced.
			bin := GinkgoT().TempDir()
			calls := GinkgoT().TempDir()
			Expect(os.WriteFile(filepath.Join(bin, "upgrader"), []byte("#!/bin/sh\nprintf '%s' \"$1\" > \"$CALLS/upgrader\"\n"), 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(bin, "chroot"), []byte("#!/bin/sh\nprintf '%s' \"$*\" > \"$CALLS/chroot\"\n"), 0o755)).To(Succeed())

			cmd := exec.Command(osUpgradeShell, "-c", script)
			cmd.Env = []string{
				"HOST=" + host,
				"CALLS=" + calls,
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			}
			out, err := cmd.CombinedOutput()
			Expect(err).ToNot(HaveOccurred(), string(out))

			upgraded, err := os.ReadFile(filepath.Join(calls, "upgrader"))
			if err != nil {
				Expect(os.IsNotExist(err)).To(BeTrue())
			}
			chroot, err := os.ReadFile(filepath.Join(calls, "chroot"))
			if err != nil {
				Expect(os.IsNotExist(err)).To(BeTrue())
			}

			return string(out), string(upgraded), string(chroot) == "/host reboot"
		}

		DescribeTable("skips the upgrade",
			func(deployment, marker, message string) {
				output, upgradedTo, rebooted := runUpgradeScript(deployment, marker)
				Expect(upgradedTo).To(BeEmpty())
				Expect(rebooted).To(BeFalse())
				Expect(output).To(ContainSubstring(message))
			},
			Entry("on a freshly installed system whose release marker names the target OS image",
				rawDeployment, fmt.Sprintf(markerFmt, osImage), "System was installed from OS image "+osImage),
			Entry("on a freshly installed system whose release marker has trailing whitespace",
				rawDeployment, "osImage: "+osImage+"  \n", "System was installed from OS image "+osImage),
			Entry("on a system already upgraded to the target OS image",
				fmt.Sprintf(ociDeploymentFmt, osImage), noMarker, "Active OS image is already at correct version"),
			Entry("on a system already upgraded to the target OS image that still has a release marker for another image",
				fmt.Sprintf(ociDeploymentFmt, osImage), fmt.Sprintf(markerFmt, "registry.example.com/elemental-os:1.0.0"),
				"Active OS image is already at correct version"),
		)

		DescribeTable("upgrades and reboots",
			func(deployment, marker string) {
				output, upgradedTo, rebooted := runUpgradeScript(deployment, marker)
				Expect(upgradedTo).To(Equal(osImage), output)
				Expect(rebooted).To(BeTrue())
			},
			Entry("a freshly installed system without a release marker", rawDeployment, noMarker),
			Entry("a freshly installed system with an empty release marker", rawDeployment, "\n"),
			Entry("a freshly installed system whose release marker is not valid",
				rawDeployment, "osImage\n  - "+osImage+"\n"),
			Entry("a freshly installed system whose release marker names another tag of the OS image",
				rawDeployment, fmt.Sprintf(markerFmt, "registry.example.com/elemental-os:1.2.2")),
			Entry("a freshly installed system whose release marker names a longer tag starting with the target tag",
				rawDeployment, fmt.Sprintf(markerFmt, osImage+"0")),
			Entry("a freshly installed system whose release marker names a shorter tag the target tag starts with",
				rawDeployment, fmt.Sprintf(markerFmt, "registry.example.com/elemental-os:1.2")),
			Entry("a freshly installed system whose release marker names the same tag in another repository",
				rawDeployment, fmt.Sprintf(markerFmt, "registry.example.com/other-os:1.2.3")),
			Entry("a freshly installed system whose release marker names the target OS image pinned to a digest",
				rawDeployment, fmt.Sprintf(markerFmt, osImage+"@sha256:2d195da213f9ae85ff9444f224aa487e9dbd3cd8891599ae022eeb5f04a17fb2")),
			Entry("a freshly installed system whose release marker names the target only as installer image",
				rawDeployment, "installerImage: "+osImage+"\n"),
			Entry("a freshly installed system whose release marker names the target OS image in a nested field",
				rawDeployment, "previous:\n  osImage: "+osImage+"\n"),
			Entry("a freshly installed system whose release marker quotes the OS image",
				rawDeployment, "osImage: \""+osImage+"\"\n"),
			Entry("a system upgraded to another OS image that still has the release marker of its installation",
				fmt.Sprintf(ociDeploymentFmt, "registry.example.com/elemental-os:1.3.0"), fmt.Sprintf(markerFmt, osImage)),
			Entry("a system upgraded to an OS image of another repository that still has the release marker of its installation",
				fmt.Sprintf(ociDeploymentFmt, "registry.example.com/other-os:9.9.9"), fmt.Sprintf(markerFmt, osImage)),
			Entry("a system upgraded to another OS image without a release marker",
				fmt.Sprintf(ociDeploymentFmt, "registry.example.com/elemental-os:1.0.0"), noMarker),
			Entry("a system without a deployment file, even if a release marker names the target OS image",
				noDeployment, fmt.Sprintf(markerFmt, osImage)),
			Entry("a system with an empty deployment file, even if a release marker names the target OS image",
				"\n", fmt.Sprintf(markerFmt, osImage)),
		)
	})

	Describe("Upgrade script", func() {
		It("uses local container images for upgrade", func() {
			script, err := parseUpgradeScript(osImage)
			Expect(err).ToNot(HaveOccurred())
			Expect(script).To(Equal(expectedUpgradeScript))
			Expect(script).ToNot(ContainSubstring("USE_LOCAL_IMAGES=false upgrader"))
		})

		It("uses remote container images for upgrade", func() {
			Expect(os.Setenv("E3CTL_FETCH_REMOTE", "true")).To(Succeed())
			DeferCleanup(os.Unsetenv, "E3CTL_FETCH_REMOTE")

			old := `upgrader "$INCOMING" && chroot /host reboot`
			new := `USE_LOCAL_IMAGES=false upgrader "$INCOMING" && chroot /host reboot`
			expectedRemoteUpgradeScript := strings.Replace(expectedUpgradeScript, old, new, 1)

			script, err := parseUpgradeScript(osImage)
			Expect(err).ToNot(HaveOccurred())
			Expect(script).To(Equal(expectedRemoteUpgradeScript))
		})
	})
})
