/*
Copyright 2026 OpenRiak Contributors.

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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/marthydavid/openriak-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "agents-riak-operator-kubernetes-lifecycle-system"

// serviceAccountName created for the project
const serviceAccountName = "agents-riak-operator-kubernetes-lifecycle-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "agents-riak-operator-kubernetes-lifecycle-metrics"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "agents-riak-operator-kubernetes-lifecycle-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// installing CRDs, and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=agents-riak-operator-kubernetes-lifecycle-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			if !skipPrometheusInstall {
				By("validating that the ServiceMonitor for Prometheus is applied in the namespace")
				cmd = exec.Command("kubectl", "get", "ServiceMonitor", "-n", namespace)
				_, err = utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "ServiceMonitor should exist")
			}

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:7.78.0",
				"--", "/bin/sh", "-c", fmt.Sprintf(
					"curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics",
					token, metricsServiceName, namespace))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	// ── Riak CR lifecycle ─────────────────────────────────────────────────────────
	// Create a RiakCluster, a RiakBucket, and a RiakUser, verify the operator
	// reconciles all three (finalizers, child resources, status phases), then
	// delete them and confirm they are fully removed.
	Context("Riak CR lifecycle", Ordered, func() {
		const (
			riakNS      = "default"
			clusterName = "e2e-cluster"
			bucketName  = "e2e-bucket"
			userName    = "e2e-user"
			secretName  = "e2e-user-secret"
		)

		// applyManifest writes YAML to a temp file and runs kubectl apply.
		applyManifest := func(yaml string) {
			f, err := os.CreateTemp("", "e2e-riak-*.yaml")
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			_, err = fmt.Fprint(f, yaml)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			ExpectWithOffset(1, f.Close()).To(Succeed())
			defer os.Remove(f.Name())
			cmd := exec.Command("kubectl", "apply", "-f", f.Name())
			_, err = utils.Run(cmd)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		BeforeAll(func() {
			By("creating a single-node RiakCluster")
			applyManifest(fmt.Sprintf(`
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: %s
  namespace: %s
spec:
  size: 1
  image: ghcr.io/marthydavid/riak:3.2.6
  storageClassName: standard
  storageSize: 1Gi
`, clusterName, riakNS))

			By("creating the password Secret for the RiakUser")
			applyManifest(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  password: e2etestpassword
`, secretName, riakNS))

			By("creating a RiakBucket")
			applyManifest(fmt.Sprintf(`
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata:
  name: %s
  namespace: %s
spec:
  clusterName: %s
  bucketName: e2e-app-data
  bucketType: default
`, bucketName, riakNS, clusterName))

			By("creating a RiakUser with read/write grants")
			applyManifest(fmt.Sprintf(`
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata:
  name: %s
  namespace: %s
spec:
  clusterName: %s
  username: e2euser
  passwordSecret:
    name: %s
    key: password
  grants:
    - resource: any
      permission: read
    - resource: any
      permission: write
`, userName, riakNS, clusterName, secretName))
		})

		AfterAll(func() {
			By("removing e2e Riak test resources (best-effort)")
			for _, args := range [][]string{
				{"kubectl", "delete", "riakuser", userName, "-n", riakNS, "--ignore-not-found"},
				{"kubectl", "delete", "riakbucket", bucketName, "-n", riakNS, "--ignore-not-found"},
				{"kubectl", "delete", "riakcluster", clusterName, "-n", riakNS, "--ignore-not-found"},
				{"kubectl", "delete", "secret", secretName, "-n", riakNS, "--ignore-not-found"},
			} {
				cmd := exec.Command(args[0], args[1:]...)
				_, _ = utils.Run(cmd)
			}
		})

		It("operator sets a finalizer and creates infrastructure for the RiakCluster", func() {
			By("verifying the operator sets a finalizer on the RiakCluster")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakcluster", clusterName,
					"-n", riakNS, "-o", "jsonpath={.metadata.finalizers[0]}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("riak.openriak.io"))
			}).Should(Succeed())

			By("verifying the operator creates the StatefulSet")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", clusterName, "-n", riakNS)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}).Should(Succeed())

			By("verifying the operator creates the headless Service")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "service", clusterName+"-headless", "-n", riakNS)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}).Should(Succeed())

			By("verifying the RiakCluster status.phase is set")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakcluster", clusterName,
					"-n", riakNS, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
			}).Should(Succeed())
		})

		It("operator sets a finalizer and a status phase for the RiakBucket", func() {
			By("verifying the operator sets a finalizer on the RiakBucket")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakbucket", bucketName,
					"-n", riakNS, "-o", "jsonpath={.metadata.finalizers[0]}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("riak.openriak.io"))
			}).Should(Succeed())

			By("verifying the RiakBucket status.phase is set")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakbucket", bucketName,
					"-n", riakNS, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
			}).Should(Succeed())
		})

		It("operator sets a finalizer and a status phase for the RiakUser", func() {
			By("verifying the operator sets a finalizer on the RiakUser")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakuser", userName,
					"-n", riakNS, "-o", "jsonpath={.metadata.finalizers[0]}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("riak.openriak.io"))
			}).Should(Succeed())

			By("verifying the RiakUser status.phase is set")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakuser", userName,
					"-n", riakNS, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
			}).Should(Succeed())
		})

		It("all three CR types appear in kubectl get", func() {
			By("listing RiakClusters")
			cmd := exec.Command("kubectl", "get", "riakclusters", "-n", riakNS)
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring(clusterName))

			By("listing RiakBuckets")
			cmd = exec.Command("kubectl", "get", "riakbuckets", "-n", riakNS)
			out, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring(bucketName))

			By("listing RiakUsers")
			cmd = exec.Command("kubectl", "get", "riakusers", "-n", riakNS)
			out, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring(userName))
		})

		It("all CRs are removed cleanly on deletion", func() {
			By("deleting the RiakUser and waiting for it to disappear")
			cmd := exec.Command("kubectl", "delete", "riakuser", userName, "-n", riakNS)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakuser", userName, "-n", riakNS)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}).Should(Succeed())

			By("deleting the RiakBucket and waiting for it to disappear")
			cmd = exec.Command("kubectl", "delete", "riakbucket", bucketName, "-n", riakNS)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakbucket", bucketName, "-n", riakNS)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}).Should(Succeed())

			By("deleting the RiakCluster and waiting for it to disappear")
			cmd = exec.Command("kubectl", "delete", "riakcluster", clusterName, "-n", riakNS)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakcluster", clusterName, "-n", riakNS)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred())
			}).Should(Succeed())
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
