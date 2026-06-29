//go:build e2e
// +build e2e

/*
Copyright 2026.

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

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/floppyfish14/agent-orc/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/agent-orc:v0.0.1"
	// routerImage is the model-router sidecar image built and loaded for tests.
	// Override via ROUTER_IMG env var; defaults to the same tag as managerImage for consistency.
	routerImage = func() string {
		if img := os.Getenv("ROUTER_IMG"); img != "" {
			return img
		}
		return "example.com/model-router:v0.0.1"
	}()
	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind and CertManager.
//
// To skip CertManager installation, set: CERT_MANAGER_INSTALL_SKIP=true
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting agent-orc e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	By("building the model-router sidecar image")
	cmd = exec.Command("make", "docker-build-model-router",
		fmt.Sprintf("ROUTER_IMG=%s", routerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the model-router image")

	By("loading the model-router sidecar image on Kind")
	err = utils.LoadImageToKindClusterWithName(routerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the model-router image into Kind")

	setupCertManager()

	By("creating manager namespace")
	cmd = exec.Command("kubectl", "create", "ns", namespace)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to create namespace")

	By("labeling the namespace to enforce the restricted security policy")
	cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
		"pod-security.kubernetes.io/enforce=restricted")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

	By("installing CRDs")
	cmd = exec.Command("make", "install")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("deploying the controller-manager")
	cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

	By("patching MODEL_ROUTER_IMAGE on controller-manager deployment")
	// TOKEN_REVIEWER_CLUSTER_ROLE is already set via kustomize patch; this only overrides
	// the router image so that pods use the locally-loaded image instead of the registry default.
	cmd = exec.Command("kubectl", "set", "env", "deployment/agent-orc-controller-manager",
		fmt.Sprintf("MODEL_ROUTER_IMAGE=%s", routerImage),
		"-n", namespace,
	)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to patch MODEL_ROUTER_IMAGE")

	By("waiting for the controller-manager deployment to be available")
	cmd = exec.Command("kubectl", "wait", "deployment",
		"agent-orc-controller-manager",
		"--for=condition=Available",
		"-n", namespace,
		"--timeout=120s",
	)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "controller-manager did not become available in time")

	By("waiting for the webhook server to accept connections")
	// The pod may report Available before port 9443 is fully bound.
	// Probe via server-side dry-run; any response (including rejection) means the webhook is up.
	Eventually(func() error {
		c := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
		c.Stdin = strings.NewReader(`
apiVersion: agentorc.agentorc.io/v1alpha1
kind: AgentRun
metadata:
  name: webhook-probe
  namespace: default
spec:
  agentRef: probe
  input: probe
`)
		out, runErr := utils.Run(c)
		if runErr == nil {
			return nil
		}
		combined := out + runErr.Error()
		// connection refused / EOF / deadline = webhook not yet ready
		if strings.Contains(combined, "connection refused") ||
			strings.Contains(combined, "EOF") ||
			strings.Contains(combined, "deadline exceeded") {
			return fmt.Errorf("webhook not yet ready: %s", combined)
		}
		// Any other error (e.g., "agent probe not found", validation rejection) = webhook is up
		return nil
	}, 60*time.Second, 2*time.Second).Should(Succeed(), "webhook did not become ready within 60s")
})

var _ = AfterSuite(func() {
	teardownCertManager()
})

// setupCertManager installs CertManager if needed for webhook tests.
// Skips installation if CERT_MANAGER_INSTALL_SKIP=true or if already present.
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	// Mark for cleanup before installation to handle interruptions and partial installs.
	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManager uninstalls CertManager if it was installed by setupCertManager.
// This ensures we only remove what we installed.
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
