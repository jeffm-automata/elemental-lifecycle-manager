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

// apiserver starts a bare Kubernetes API server (envtest: etcd and kube-apiserver, nothing else)
// with the NodeImage CRD installed and writes an admin kubeconfig for it. It exists to run the
// operator against a management API that has no other controller or resource definition in it.
// Usage: KUBEBUILDER_ASSETS=<dir> apiserver <crd-dir> <kubeconfig-file>; stops on SIGINT/SIGTERM.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: apiserver <crd-dir> <kubeconfig-file>")
		os.Exit(2)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{os.Args[1]}, ErrorIfCRDPathMissing: true}
	if _, err := env.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	code := 0
	if err := writeKubeconfig(env, os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "kubeconfig:", err)
		code = 1
	} else {
		fmt.Println("ready", os.Args[2])
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
	}
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stop:", err)
	}
	os.Exit(code)
}

func writeKubeconfig(env *envtest.Environment, path string) error {
	user, err := env.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		return err
	}
	kubeconfig, err := user.KubeConfig()
	if err != nil {
		return err
	}
	return os.WriteFile(path, kubeconfig, 0o600)
}
