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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/utils/ptr"
)

var _ = Describe("OS plans that upgrade Kubernetes in the same pass", func() {
	const (
		releaseName    = "test-release"
		releaseVersion = "0.6.0"
		osImage        = "registry.example.com/elemental-os:1.2.3"
		osVersion      = "1.2.3"
		k8sVersion     = "v1.37.2+rke2r1"
		binPath        = "/opt/rke2/bin/rke2"
		oldBinary      = "#!/bin/sh\necho 'rke2 version v1.37.1+rke2r1 (old)'\n"
		newBinary      = "#!/bin/sh\necho 'rke2 version v1.37.2+rke2r1 (new)'\n"
		ociDeployment  = "sourceOS:\n    uri: oci://%s\n"
	)

	Describe("Plans", func() {
		It("keep everything of the OS plans but the prepare step and the upgrade script", func() {
			plain, err := OSControlPlane(releaseName, osImage, osVersion, releaseVersion, true)
			Expect(err).ToNot(HaveOccurred())
			onePass, err := OSControlPlaneOnePass(releaseName, osImage, osVersion, releaseVersion, k8sVersion, true)
			Expect(err).ToNot(HaveOccurred())

			Expect(onePass.Name).To(Equal(plain.Name))
			Expect(onePass.Labels).To(Equal(plain.Labels))
			Expect(onePass.Spec.Version).To(Equal(plain.Spec.Version))
			Expect(onePass.Spec.Concurrency).To(Equal(int64(1)))
			Expect(onePass.Spec.NodeSelector).To(Equal(plain.Spec.NodeSelector))
			Expect(onePass.Spec.Drain).To(Equal(plain.Spec.Drain))
			Expect(onePass.Spec.JobActiveDeadlineSecs).To(Equal(plain.Spec.JobActiveDeadlineSecs))
			Expect(onePass.Spec.Upgrade.Image).To(Equal(osImage))
			Expect(onePass.Annotations).To(HaveKeyWithValue(OnePassAnnotation, k8sVersion))
			Expect(plain.Annotations).ToNot(HaveKey(OnePassAnnotation))
		})

		It("stage the Kubernetes binary from the image of the Kubernetes upgrade before the node is drained", func() {
			cp, err := OSControlPlaneOnePass(releaseName, osImage, osVersion, releaseVersion, k8sVersion, true)
			Expect(err).ToNot(HaveOccurred())

			Expect(cp.Spec.Prepare).ToNot(BeNil())
			// SUC would otherwise tag the image with the version of the plan, which is the OS version.
			Expect(cp.Spec.Prepare.Image).To(Equal("rancher/rke2-upgrade:v1.37.2-rke2r1"))
			Expect(cp.Spec.Prepare.Command).To(Equal([]string{osUpgradeShell, "-c"}))
			Expect(cp.Spec.Prepare.SecurityContext).ToNot(BeNil())
			Expect(ptr.Deref(cp.Spec.Prepare.SecurityContext.Privileged, false)).To(BeTrue())

			stage := cp.Spec.Prepare.Args[0]
			Expect(stage).To(ContainSubstring(`KUBERNETES_VERSION="v1.37.2+rke2r1"`))
			Expect(stage).To(ContainSubstring(`OS_IMAGE="` + osImage + `"`))
			Expect(stage).ToNot(ContainSubstring("upgrader"))
			Expect(stage).ToNot(ContainSubstring("host reboot"))
			Expect(stage).ToNot(ContainSubstring("kill"))
			Expect(stage).ToNot(ContainSubstring("get nodes"))

			Expect(cp.Spec.Upgrade.Args).To(HaveLen(1))
			Expect(cp.Spec.Upgrade.Args[0]).To(ContainSubstring(`upgrader "$INCOMING" && install_staged_kubernetes && chroot /host reboot`))
		})

		It("make a worker node look at the control plane nodes before it stages anything", func() {
			worker, err := OSWorkerOnePass(releaseName, osImage, osVersion, releaseVersion, k8sVersion, true, 2)
			Expect(err).ToNot(HaveOccurred())

			Expect(worker.Name).To(Equal("elemental-os-worker-1-2-3"))
			Expect(worker.Spec.Concurrency).To(Equal(int64(2)))
			Expect(worker.Spec.Prepare.Image).To(Equal("rancher/rke2-upgrade:v1.37.2-rke2r1"))
			Expect(worker.Spec.Prepare.Args[0]).To(ContainSubstring("get nodes --selector='node-role.kubernetes.io/control-plane'"))
		})

		It("leave the script of the plans without the option as it is", func() {
			plain, err := parseUpgradeScript(osImage)
			Expect(err).ToNot(HaveOccurred())
			Expect(plain).ToNot(ContainSubstring("staged"))
			Expect(plain).ToNot(ContainSubstring("KUBERNETES_MARKER"))
			Expect(plain).To(HaveSuffix("upgrader \"$INCOMING\" && chroot /host reboot\n"))
		})
	})

	// A fake node: the rke2 binary in use, optionally the deployment file that says which OS image runs.
	newHost := func(deployment string) string {
		host := GinkgoT().TempDir()
		Expect(os.MkdirAll(filepath.Join(host, "opt", "rke2", "bin"), 0o755)).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(host, "opt", "rke2", "lib", "systemd", "system"), 0o755)).To(Succeed())
		Expect(os.MkdirAll(filepath.Join(host, "etc", "elemental"), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(host, binPath), []byte(oldBinary), 0o755)).To(Succeed())
		if deployment != "" {
			Expect(os.WriteFile(filepath.Join(host, "etc", "elemental", "deployment.yaml"), []byte(deployment), 0o444)).To(Succeed())
		}
		return host
	}

	read := func(path string) string {
		content, err := os.ReadFile(path)
		if err != nil {
			Expect(os.IsNotExist(err)).To(BeTrue())
			return ""
		}
		return string(content)
	}

	// runStage runs the prepare step against the fake node. controlPlaneVersions is what kubectl prints for the control plane nodes.
	runStage := func(host, binary string, worker bool, controlPlaneVersions string) (string, error) {
		script, err := parseKubernetesStageScript(osImage, k8sVersion, worker)
		Expect(err).ToNot(HaveOccurred())

		tools := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(tools, "rke2-new"), []byte(binary), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tools, "kubectl"), []byte("#!/bin/sh\nprintf '"+controlPlaneVersions+"'\n"), 0o755)).To(Succeed())

		cmd := exec.Command(osUpgradeShell, "-c", script)
		cmd.Env = []string{
			"HOST=" + host,
			"NEW_BINARY=" + filepath.Join(tools, "rke2-new"),
			"RKE2_BIN_PATH=" + binPath,
			"KUBECTL=" + filepath.Join(tools, "kubectl"),
			"PATH=" + os.Getenv("PATH"),
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// runUpgrade runs the upgrade step against the fake node and reports whether the OS upgrade ran and whether the node was rebooted.
	runUpgrade := func(host string, upgraderFails bool) (output string, upgraded, rebooted bool, err error) {
		script, parseErr := parseUpgradeScriptFor(osImage, true)
		Expect(parseErr).ToNot(HaveOccurred())

		bin := GinkgoT().TempDir()
		calls := GinkgoT().TempDir()
		upgrader := "#!/bin/sh\nprintf '%s' \"$1\" > \"$CALLS/upgrader\"\n"
		if upgraderFails {
			upgrader += "exit 1\n"
		}
		Expect(os.WriteFile(filepath.Join(bin, "upgrader"), []byte(upgrader), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(bin, "chroot"), []byte("#!/bin/sh\nprintf '%s' \"$*\" > \"$CALLS/chroot\"\n"), 0o755)).To(Succeed())

		cmd := exec.Command(osUpgradeShell, "-c", script)
		cmd.Env = []string{
			"HOST=" + host,
			"CALLS=" + calls,
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		}
		out, err := cmd.CombinedOutput()

		return string(out), read(filepath.Join(calls, "upgrader")) == osImage, read(filepath.Join(calls, "chroot")) == "/host reboot", err
	}

	Describe("Prepare step", func() {
		DescribeTable("finds the binary of the running rke2 process",
			func(cmdline string) {
				host := newHost("")
				Expect(os.MkdirAll(filepath.Join(host, "proc", "4242"), 0o755)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(host, "proc", "4242", "cmdline"), []byte(cmdline), 0o444)).To(Succeed())

				script, err := parseKubernetesStageScript(osImage, k8sVersion, false)
				Expect(err).ToNot(HaveOccurred())

				// The process list as the image of the Kubernetes upgrade prints it: the PID comes first.
				tools := GinkgoT().TempDir()
				ps := "#!/bin/sh\necho 'PID   USER     TIME  COMMAND'\necho '    1 root      0:03 /usr/lib/systemd/systemd'\necho ' 4242 root      1:07 /opt/rke2/bin/rke2 server'\n"
				Expect(os.WriteFile(filepath.Join(tools, "ps"), []byte(ps), 0o755)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(tools, "rke2-new"), []byte(newBinary), 0o755)).To(Succeed())

				cmd := exec.Command(osUpgradeShell, "-c", script)
				cmd.Env = []string{
					"HOST=" + host,
					"NEW_BINARY=" + filepath.Join(tools, "rke2-new"),
					"PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH"),
				}
				out, err := cmd.CombinedOutput()
				Expect(err).ToNot(HaveOccurred(), string(out))

				Expect(read(filepath.Join(host, binPath+".elemental-staged"))).To(Equal(newBinary))
				Expect(read(filepath.Join(host, "run", "elemental-onepass"))).To(HavePrefix("/opt/rke2/bin/rke2 "))
			},
			// rke2 rewrites its command line to one string, padded with NUL bytes.
			Entry("when the command line is one string", "/opt/rke2/bin/rke2 server\x00\x00\x00\x00"),
			Entry("when the arguments of the command line are separated by NUL bytes", "/opt/rke2/bin/rke2\x00server\x00"),
		)

		It("puts the new binary next to the one in use and changes nothing else", func() {
			host := newHost("")
			out, err := runStage(host, newBinary, false, "")
			Expect(err).ToNot(HaveOccurred(), out)

			Expect(read(filepath.Join(host, binPath))).To(Equal(oldBinary))
			Expect(read(filepath.Join(host, binPath+".elemental-staged"))).To(Equal(newBinary))
			Expect(read(filepath.Join(host, "run", "elemental-onepass"))).To(MatchRegexp(`^/opt/rke2/bin/rke2 [0-9a-f]{64} v1\.37\.2\+rke2r1\n$`))
			Expect(out).To(ContainSubstring("is staged"))
		})

		It("stages nothing on a node that has the binary already, and removes what an earlier attempt left", func() {
			host := newHost("")
			Expect(os.WriteFile(filepath.Join(host, binPath+".elemental-staged"), []byte("stale"), 0o755)).To(Succeed())
			Expect(os.MkdirAll(filepath.Join(host, "run"), 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(host, "run", "elemental-onepass"), []byte("stale\n"), 0o644)).To(Succeed())

			out, err := runStage(host, oldBinary, false, "")
			Expect(err).ToNot(HaveOccurred(), out)

			Expect(out).To(ContainSubstring("Nothing is staged"))
			Expect(filepath.Join(host, binPath+".elemental-staged")).ToNot(BeAnExistingFile())
			Expect(filepath.Join(host, "run", "elemental-onepass")).ToNot(BeAnExistingFile())
		})

		It("refuses a node that runs a newer version", func() {
			host := newHost("")
			out, err := runStage(host, "#!/bin/sh\necho 'rke2 version v1.36.9+rke2r1 (older)'\n", false, "")
			Expect(err).To(HaveOccurred(), out)
			Expect(out).To(ContainSubstring("newer rke2"))
			Expect(filepath.Join(host, binPath+".elemental-staged")).ToNot(BeAnExistingFile())
		})

		It("stages on a worker node when all control plane nodes report the target version", func() {
			host := newHost("")
			out, err := runStage(host, newBinary, true, `v1.37.2+rke2r1\nv1.37.2+rke2r1\nv1.37.2+rke2r1\n`)
			Expect(err).ToNot(HaveOccurred(), out)
			Expect(read(filepath.Join(host, binPath+".elemental-staged"))).To(Equal(newBinary))
		})

		It("stages nothing on a worker node while a control plane node reports another version", func() {
			host := newHost("")
			out, err := runStage(host, newBinary, true, `v1.37.2+rke2r1\nv1.37.1+rke2r1\nv1.37.2+rke2r1\n`)
			Expect(err).ToNot(HaveOccurred(), out)
			Expect(out).To(ContainSubstring("Nothing is staged"))
			Expect(filepath.Join(host, binPath+".elemental-staged")).ToNot(BeAnExistingFile())
			Expect(filepath.Join(host, "run", "elemental-onepass")).ToNot(BeAnExistingFile())
		})
	})

	Describe("Upgrade step", func() {
		It("installs the staged binary after the OS upgrade and before the reboot, and keeps the one it replaces", func() {
			host := newHost("")
			out, err := runStage(host, newBinary, false, "")
			Expect(err).ToNot(HaveOccurred(), out)

			out, upgraded, rebooted, err := runUpgrade(host, false)
			Expect(err).ToNot(HaveOccurred(), out)
			Expect(upgraded).To(BeTrue())
			Expect(rebooted).To(BeTrue())

			Expect(read(filepath.Join(host, binPath))).To(Equal(newBinary))
			Expect(read(filepath.Join(host, binPath+".elemental-previous"))).To(Equal(oldBinary))
			Expect(filepath.Join(host, binPath+".elemental-staged")).ToNot(BeAnExistingFile())
			Expect(filepath.Join(host, "run", "elemental-onepass")).ToNot(BeAnExistingFile())
			Expect(read(filepath.Join(host, "opt", "rke2", "lib", "systemd", "system", "rke2-server.env"))).To(ContainSubstring("HOME=/root"))
			Expect(out).To(ContainSubstring("Kubernetes v1.37.2+rke2r1 is installed"))
		})

		It("reboots a node on which nothing is staged without touching its Kubernetes binary", func() {
			host := newHost("")
			out, upgraded, rebooted, err := runUpgrade(host, false)
			Expect(err).ToNot(HaveOccurred(), out)
			Expect(upgraded).To(BeTrue())
			Expect(rebooted).To(BeTrue())
			Expect(read(filepath.Join(host, binPath))).To(Equal(oldBinary))
			Expect(filepath.Join(host, binPath+".elemental-previous")).ToNot(BeAnExistingFile())
		})

		It("does not touch the Kubernetes binary and does not reboot when the OS upgrade fails", func() {
			host := newHost("")
			out, err := runStage(host, newBinary, false, "")
			Expect(err).ToNot(HaveOccurred(), out)

			out, upgraded, rebooted, err := runUpgrade(host, true)
			Expect(err).To(HaveOccurred(), out)
			Expect(upgraded).To(BeTrue())
			Expect(rebooted).To(BeFalse())
			Expect(read(filepath.Join(host, binPath))).To(Equal(oldBinary))
		})

		It("fails without a reboot when the staged binary is gone or was changed", func() {
			host := newHost("")
			out, err := runStage(host, newBinary, false, "")
			Expect(err).ToNot(HaveOccurred(), out)
			Expect(os.WriteFile(filepath.Join(host, binPath+".elemental-staged"), []byte("something else"), 0o755)).To(Succeed())

			out, _, rebooted, err := runUpgrade(host, false)
			Expect(err).To(HaveOccurred(), out)
			Expect(rebooted).To(BeFalse())
			Expect(out).To(ContainSubstring("is not the one that was staged"))
			Expect(read(filepath.Join(host, binPath))).To(Equal(oldBinary))

			Expect(os.Remove(filepath.Join(host, binPath+".elemental-staged"))).To(Succeed())
			out, _, rebooted, err = runUpgrade(host, false)
			Expect(err).To(HaveOccurred(), out)
			Expect(rebooted).To(BeFalse())
			Expect(out).To(ContainSubstring("is missing"))
		})

		It("leaves Kubernetes to the Kubernetes phase on a node whose OS is on the target already", func() {
			host := newHost(fmt.Sprintf(ociDeployment, osImage))
			out, err := runStage(host, newBinary, false, "")
			Expect(err).ToNot(HaveOccurred(), out)

			out, upgraded, rebooted, err := runUpgrade(host, false)
			Expect(err).ToNot(HaveOccurred(), out)
			Expect(upgraded).To(BeFalse())
			Expect(rebooted).To(BeFalse())
			Expect(out).To(ContainSubstring("left to the Kubernetes upgrade phase"))
			Expect(read(filepath.Join(host, binPath))).To(Equal(oldBinary))
			Expect(filepath.Join(host, binPath+".elemental-staged")).ToNot(BeAnExistingFile())
			Expect(filepath.Join(host, "run", "elemental-onepass")).ToNot(BeAnExistingFile())
		})
	})
})
