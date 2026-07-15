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
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/marthydavid/openriak-operator/test/utils"
)

// collectDiagnostics gathers logs and events from the operator and Riak operand pods
// and writes them to logDir so they can be uploaded as CI artifacts.
func collectDiagnostics(controllerPodName string) {
	collectDiagnosticsTo(logDir, controllerPodName)
}

// collectDiagnosticsTo writes the diagnostic snapshot into dir. AfterAll cleanup
// hooks use a subdirectory so the pre-deletion state survives the later AfterEach
// collection, which overwrites the files at the logDir root.
func collectDiagnosticsTo(dir, controllerPodName string) {
	_ = os.MkdirAll(dir, 0o755)
	run := func(args ...string) string {
		out, _ := utils.Run(exec.Command(args[0], args[1:]...))
		return out
	}
	write := func(name, content string) {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}

	write("operator.log", run("kubectl", "logs", controllerPodName, "-n", namespace, "--tail=1000"))
	write("riak-pods.log", run("kubectl", "logs", "-n", "default", "-l", "app=riak", "--tail=500", "--prefix=true"))
	write("riak-pods-describe.log", run("kubectl", "describe", "pods", "-n", "default", "-l", "app=riak"))
	write("events-default.log", run("kubectl", "get", "events", "-n", "default", "--sort-by=.lastTimestamp"))
	write("events-operator.log", run("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp"))
	write("riakclusters.log", run("kubectl", "get", "riakclusters", "-A", "-o", "yaml"))
	write("riakbuckets.log", run("kubectl", "get", "riakbuckets", "-A", "-o", "yaml"))
	write("riakusers.log", run("kubectl", "get", "riakusers", "-A", "-o", "yaml"))
}

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

	// CRD install and controller deploy happen once at suite scope (BeforeSuite),
	// not here: Ginkgo randomizes top-level container order, so a per-Describe
	// teardown would remove the operator/CRDs before the other Describes run.

	// After each test, always collect diagnostics to logDir for CI artifact upload.
	// On failure, also echo key logs to GinkgoWriter for immediate visibility.
	AfterEach(func() {
		collectDiagnostics(controllerPodName)

		specReport := CurrentSpecReport()
		if specReport.Failed() {
			if logs, err := os.ReadFile(filepath.Join(logDir, "operator.log")); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "=== operator logs ===\n%s\n", logs)
			}
			if logs, err := os.ReadFile(filepath.Join(logDir, "events-default.log")); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "=== events (default) ===\n%s\n", logs)
			}
			if logs, err := os.ReadFile(filepath.Join(logDir, "riak-pods.log")); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "=== riak pod logs ===\n%s\n", logs)
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
			issuerName  = "e2e-user-issuer"
		)

		// applyManifest writes YAML to a temp file and runs kubectl apply.
		applyManifest := func(yaml string) {
			f, err := os.CreateTemp("", "e2e-riak-*.yaml")
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			_, err = fmt.Fprint(f, yaml)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			ExpectWithOffset(1, f.Close()).To(Succeed())
			defer func() { _ = os.Remove(f.Name()) }()
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
  riakConfig:
    ring_size: "8"
`, clusterName, riakNS))

			By("creating a self-signed Issuer for the RiakUser client certificate")
			applyManifest(fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
  namespace: %s
spec:
  selfSigned: {}
`, issuerName, riakNS))

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
  bucketType: e2e-app-type
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
  certificateRef:
    issuerRef:
      name: %s
      kind: Issuer
  grants:
    - resource: any
      permission: read
    - resource: any
      permission: write
`, userName, riakNS, clusterName, issuerName))
		})

		AfterAll(func() {
			// Snapshot state before deleting anything: this AfterAll runs before the
			// outer AfterEach, which would otherwise only capture post-deletion state.
			collectDiagnosticsTo(filepath.Join(logDir, "pre-cleanup-lifecycle"), controllerPodName)

			By("removing e2e Riak test resources (best-effort)")
			for _, args := range [][]string{
				{"kubectl", "delete", "riakuser", userName, "-n", riakNS, "--ignore-not-found"},
				{"kubectl", "delete", "riakbucket", bucketName, "-n", riakNS, "--ignore-not-found"},
				{"kubectl", "delete", "riakcluster", clusterName, "-n", riakNS, "--ignore-not-found"},
				{"kubectl", "delete", "issuer", issuerName, "-n", riakNS, "--ignore-not-found"},
				{"kubectl", "delete", "secret", userName + "-client-tls", "-n", riakNS, "--ignore-not-found"},
				{"kubectl", "delete", "riakcluster", "e2e-default-image", "-n", riakNS, "--ignore-not-found"},
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

		It("RiakCluster reaches Ready phase once Riak pod is running", func() {
			// Riak needs to pull its image and pass liveness/readiness probes before
			// the operator transitions to Ready. Allow up to 5 minutes.
			By("waiting for RiakCluster status.phase == Ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakcluster", clusterName,
					"-n", riakNS, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Ready"))
			}, 5*time.Minute, 10*time.Second).Should(Succeed())

			By("verifying readyNodes equals the cluster size")
			cmd := exec.Command("kubectl", "get", "riakcluster", clusterName,
				"-n", riakNS, "-o", "jsonpath={.status.readyNodes}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("1"))
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

		It("RiakBucket reaches Ready phase once the cluster is Ready", func() {
			// The bucket controller only reconciles when cluster.status.phase == Ready,
			// so this check is meaningful only after the cluster Ready test passes.
			By("waiting for RiakBucket status.phase == Ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakbucket", bucketName,
					"-n", riakNS, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Ready"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
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

		It("RiakUser reaches Ready phase once the cluster is Ready", func() {
			By("waiting for RiakUser status.phase == Ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "riakuser", userName,
					"-n", riakNS, "-o", "jsonpath={.status.phase}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Ready"))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("RiakBucket exists in the Riak cluster after CR reaches Ready", func() {
			// The operator materialises a RiakBucket CR as a Riak bucket *type*
			// named spec.bucketType (buckets themselves only exist on first
			// write), so the Riak-side check is for the type.
			By("verifying the bucket type exists via riak-admin bucket-type list")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", "-n", riakNS, clusterName+"-0", "--",
					"riak-admin", "bucket-type", "list")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to list bucket types")
				g.Expect(out).To(ContainSubstring("e2e-app-type"),
					"Bucket type e2e-app-type not found in riak-admin bucket-type list")
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("RiakUser exists in the Riak cluster after CR reaches Ready", func() {
			By("verifying the user exists via riak-admin security listing")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", "-n", riakNS, clusterName+"-0", "--",
					"riak-admin", "security", "print-users")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to list users")
				if !strings.Contains(out, "e2euser") {
					// Log the RiakUser CR status for debugging
					statusCmd := exec.Command("kubectl", "get", "riakuser", userName,
						"-n", riakNS, "-o", "yaml")
					statusOut, _ := utils.Run(statusCmd)
					fmt.Printf("RiakUser status:\n%s\n", statusOut)
				}
				g.Expect(out).To(ContainSubstring("e2euser"),
					"User e2euser not found in riak-admin security list users")
			}, 5*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("RiakUser grants are applied correctly in the Riak cluster", func() {
			By("verifying the user has read/write grants via riak-admin security listing")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", "-n", riakNS, clusterName+"-0", "--",
					"riak-admin", "security", "print-grants", "e2euser")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to print user grants")
				// Verify that grants include read and write permissions
				// Check for either riak_kv.get (newer format) or read (legacy format)
				g.Expect(out).To(Or(
					ContainSubstring("riak_kv.get"),
					ContainSubstring("read"),
				))
				// Check for either riak_kv.put (newer format) or write (legacy format)
				g.Expect(out).To(Or(
					ContainSubstring("riak_kv.put"),
					ContainSubstring("write"),
				))
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
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

		It("operator uses --riak-image default when spec.image is omitted", func() {
			const defaultImageCluster = "e2e-default-image"

			By("creating a RiakCluster without spec.image")
			applyManifest(fmt.Sprintf(`
apiVersion: riak.openriak.io/v1
kind: RiakCluster
metadata:
  name: %s
  namespace: %s
spec:
  size: 1
  storageClassName: standard
  storageSize: 1Gi
  riakConfig:
    ring_size: "8"
`, defaultImageCluster, riakNS))

			By("verifying the StatefulSet uses the operator default image")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", defaultImageCluster,
					"-n", riakNS, "-o", "jsonpath={.spec.template.spec.containers[0].image}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("ghcr.io/marthydavid/riak:3.2.6"))
			}).Should(Succeed())

			By("cleaning up the default-image cluster")
			cmd := exec.Command("kubectl", "delete", "riakcluster", defaultImageCluster, "-n", riakNS)
			_, _ = utils.Run(cmd)
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

// ── mTLS with cert-manager ────────────────────────────────────────────────────
// Creates a self-signed CA chain via cert-manager, then creates a RiakCluster
// with TLS enabled and a RiakUser using certificate-based auth. Verifies that
// cert-manager Certificate objects are created and the StatefulSet is configured
// with the TLS volume/mount/port.
var _ = Describe("Riak mTLS", Ordered, func() {
	const (
		tlsNS            = "default"
		tlsClusterName   = "e2e-tls-cluster"
		tlsUserName      = "e2e-tls-user"
		selfSignedIssuer = "e2e-selfsigned"
		caSecretName     = "e2e-riak-ca-secret"
		caCertName       = "e2e-riak-ca"
		caIssuerName     = "e2e-ca-issuer"
	)

	applyManifest := func(yaml string) {
		f, err := os.CreateTemp("", "e2e-tls-*.yaml")
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		_, err = fmt.Fprint(f, yaml)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, f.Close()).To(Succeed())
		defer func() { _ = os.Remove(f.Name()) }()
		cmd := exec.Command("kubectl", "apply", "-f", f.Name())
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	BeforeAll(func() {
		By("checking cert-manager CRD is installed")
		cmd := exec.Command("kubectl", "get", "crd", "certificates.cert-manager.io")
		_, err := utils.Run(cmd)
		if err != nil {
			Skip("cert-manager not installed — skipping mTLS tests")
		}

		By("creating self-signed Issuer")
		applyManifest(fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
  namespace: %s
spec:
  selfSigned: {}
`, selfSignedIssuer, tlsNS))

		By("creating a self-signed CA Certificate")
		applyManifest(fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
  namespace: %s
spec:
  isCA: true
  commonName: riak-e2e-ca
  secretName: %s
  issuerRef:
    name: %s
    kind: Issuer
`, caCertName, tlsNS, caSecretName, selfSignedIssuer))

		By("waiting for the CA Secret to be created")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "secret", caSecretName, "-n", tlsNS)
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("creating CA-backed Issuer")
		applyManifest(fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
  namespace: %s
spec:
  ca:
    secretName: %s
`, caIssuerName, tlsNS, caSecretName))

		By("creating a RiakCluster with TLS enabled")
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
  riakConfig:
    ring_size: "8"
  tls:
    enabled: true
    certManager:
      issuerName: %s
`, tlsClusterName, tlsNS, caIssuerName))

		By("creating a RiakUser with certificate auth")
		applyManifest(fmt.Sprintf(`
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata:
  name: %s
  namespace: %s
spec:
  clusterName: %s
  username: e2etlsuser
  certificateRef:
    issuerRef:
      name: %s
      kind: Issuer
`, tlsUserName, tlsNS, tlsClusterName, caIssuerName))
	})

	AfterAll(func() {
		// controllerPodName from the Manager suite is out of scope here; look it up.
		podName, _ := utils.Run(exec.Command("kubectl", "get", "pods",
			"-l", "control-plane=controller-manager", "-n", namespace,
			"-o", "jsonpath={.items[0].metadata.name}"))
		collectDiagnosticsTo(filepath.Join(logDir, "pre-cleanup-mtls"), podName)

		By("removing mTLS e2e resources (best-effort)")
		for _, args := range [][]string{
			{"kubectl", "delete", "riakuser", tlsUserName, "-n", tlsNS, "--ignore-not-found"},
			{"kubectl", "delete", "riakcluster", tlsClusterName, "-n", tlsNS, "--ignore-not-found"},
			{"kubectl", "delete", "issuer", caIssuerName, "-n", tlsNS, "--ignore-not-found"},
			{"kubectl", "delete", "certificate", caCertName, "-n", tlsNS, "--ignore-not-found"},
			{"kubectl", "delete", "issuer", selfSignedIssuer, "-n", tlsNS, "--ignore-not-found"},
			{"kubectl", "delete", "secret", caSecretName, "-n", tlsNS, "--ignore-not-found"},
		} {
			cmd := exec.Command(args[0], args[1:]...)
			_, _ = utils.Run(cmd)
		}
	})

	It("operator creates a cert-manager Certificate for the RiakCluster", func() {
		By("verifying cert-manager Certificate for the cluster exists")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "certificate", tlsClusterName+"-tls", "-n", tlsNS)
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
		}).Should(Succeed())
	})

	It("operator creates a cert-manager Certificate for the RiakUser", func() {
		By("verifying cert-manager Certificate for the user exists")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "certificate", tlsUserName+"-client-tls", "-n", tlsNS)
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
		}).Should(Succeed())
	})

	It("StatefulSet has TLS volume and https port", func() {
		By("verifying the riak-tls volume is present on the StatefulSet")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "statefulset", tlsClusterName,
				"-n", tlsNS, "-o", "jsonpath={.spec.template.spec.volumes[*].name}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("riak-tls"))
		}).Should(Succeed())

		By("verifying the https container port is present on the StatefulSet")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "statefulset", tlsClusterName,
				"-n", tlsNS, "-o", "jsonpath={.spec.template.spec.containers[0].ports[*].name}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("https"))
		}).Should(Succeed())
	})

	It("operator sets a finalizer on the TLS RiakCluster", func() {
		By("verifying the operator sets a finalizer on the TLS RiakCluster")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "riakcluster", tlsClusterName,
				"-n", tlsNS, "-o", "jsonpath={.metadata.finalizers[0]}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("riak.openriak.io"))
		}).Should(Succeed())
	})

	It("operator sets a finalizer on the certificate RiakUser", func() {
		By("verifying the operator sets a finalizer on the certificate RiakUser")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "riakuser", tlsUserName,
				"-n", tlsNS, "-o", "jsonpath={.metadata.finalizers[0]}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("riak.openriak.io"))
		}).Should(Succeed())
	})
})

// ── Certificate-auth bucket writability (protobuf) ─────────────────────────────
// End-to-end exercise of the certificate-based auth path over the Protocol
// Buffers interface: a TLS-enabled cluster, a bucket TYPE "e2e", and a RiakUser
// "e2e" authenticated by a client certificate whose CN equals the username.
// After the operator grants the user read/write on the bucket type, a helper
// script performs an authenticated protobuf write + read to prove the bucket is
// reachable and writable.
var _ = Describe("Riak certificate-auth bucket writability", Ordered, func() {
	const (
		cbNS         = "default"
		cbCluster    = "e2e-certbucket-cluster"
		cbBucketCR   = "e2e-certbucket"
		cbBucketType = "e2e"        // bucket type created + activated by the operator
		cbBucketName = "e2e-verify" // bucket within the type used for the write check
		cbUser       = "e2e"        // RiakUser CR name, Riak username, and cert CN
		cbSelfSigned = "e2e-cb-selfsigned"
		cbCACert     = "e2e-cb-ca"
		cbCASecret   = "e2e-cb-ca-secret"
		cbCAIssuer   = "e2e-cb-ca-issuer"
	)
	// cert-manager stores the user's client certificate here (default naming).
	clientCertSecret := cbUser + "-client-tls"

	applyManifest := func(yaml string) {
		f, err := os.CreateTemp("", "e2e-certbucket-*.yaml")
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		_, err = fmt.Fprint(f, yaml)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, f.Close()).To(Succeed())
		defer func() { _ = os.Remove(f.Name()) }()
		cmd := exec.Command("kubectl", "apply", "-f", f.Name())
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	BeforeAll(func() {
		By("checking cert-manager CRD is installed")
		cmd := exec.Command("kubectl", "get", "crd", "certificates.cert-manager.io")
		if _, err := utils.Run(cmd); err != nil {
			Skip("cert-manager not installed — skipping certificate-auth bucket tests")
		}

		By("creating a self-signed Issuer and CA")
		applyManifest(fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
  namespace: %s
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
  namespace: %s
spec:
  isCA: true
  commonName: openriak-certbucket-ca
  secretName: %s
  issuerRef:
    name: %s
    kind: Issuer
`, cbSelfSigned, cbNS, cbCACert, cbNS, cbCASecret, cbSelfSigned))

		By("waiting for the CA Secret to exist")
		Eventually(func(g Gomega) {
			c := exec.Command("kubectl", "get", "secret", cbCASecret, "-n", cbNS)
			_, err := utils.Run(c)
			g.Expect(err).NotTo(HaveOccurred())
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("creating a CA-backed Issuer")
		applyManifest(fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: %s
  namespace: %s
spec:
  ca:
    secretName: %s
`, cbCAIssuer, cbNS, cbCASecret))

		By("creating a TLS-enabled RiakCluster")
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
  riakConfig:
    ring_size: "8"
  tls:
    enabled: true
    certManager:
      issuerName: %s
`, cbCluster, cbNS, cbCAIssuer))

		By("creating a RiakBucket that provisions bucket type " + cbBucketType)
		applyManifest(fmt.Sprintf(`
apiVersion: riak.openriak.io/v1
kind: RiakBucket
metadata:
  name: %s
  namespace: %s
spec:
  clusterName: %s
  bucketName: %s
  bucketType: %s
`, cbBucketCR, cbNS, cbCluster, cbBucketName, cbBucketType))

		By("creating a RiakUser with certificate auth and grants on the bucket type")
		// username == cbUser drives the client cert CN, which must match for
		// certificate-based authentication. The grants map to riak_kv.get /
		// riak_kv.put on bucket type "e2e".
		applyManifest(fmt.Sprintf(`
apiVersion: riak.openriak.io/v1
kind: RiakUser
metadata:
  name: %s
  namespace: %s
spec:
  clusterName: %s
  username: %s
  certificateRef:
    issuerRef:
      name: %s
      kind: Issuer
  grants:
    - resource: bucket
      bucketName: %s
      permission: read
    - resource: bucket
      bucketName: %s
      permission: write
`, cbUser, cbNS, cbCluster, cbUser, cbCAIssuer, cbBucketType, cbBucketType))
	})

	AfterAll(func() {
		podName, _ := utils.Run(exec.Command("kubectl", "get", "pods",
			"-l", "control-plane=controller-manager", "-n", namespace,
			"-o", "jsonpath={.items[0].metadata.name}"))
		collectDiagnosticsTo(filepath.Join(logDir, "pre-cleanup-certbucket"), podName)

		By("removing certificate-auth bucket resources (best-effort)")
		for _, args := range [][]string{
			{"kubectl", "delete", "riakuser", cbUser, "-n", cbNS, "--ignore-not-found"},
			{"kubectl", "delete", "riakbucket", cbBucketCR, "-n", cbNS, "--ignore-not-found"},
			{"kubectl", "delete", "riakcluster", cbCluster, "-n", cbNS, "--ignore-not-found"},
			{"kubectl", "delete", "issuer", cbCAIssuer, "-n", cbNS, "--ignore-not-found"},
			{"kubectl", "delete", "certificate", cbCACert, "-n", cbNS, "--ignore-not-found"},
			{"kubectl", "delete", "issuer", cbSelfSigned, "-n", cbNS, "--ignore-not-found"},
			{"kubectl", "delete", "secret", cbCASecret, clientCertSecret, "-n", cbNS, "--ignore-not-found"},
		} {
			_, _ = utils.Run(exec.Command(args[0], args[1:]...))
		}
	})

	It("the TLS RiakCluster reaches Ready", func() {
		By("waiting for status.phase == Ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "riakcluster", cbCluster,
				"-n", cbNS, "-o", "jsonpath={.status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("Ready"))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("the RiakBucket provisions bucket type "+cbBucketType, func() {
		By("waiting for RiakBucket status.phase == Ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "riakbucket", cbBucketCR,
				"-n", cbNS, "-o", "jsonpath={.status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("Ready"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("the certificate-auth RiakUser reaches Ready", func() {
		By("waiting for RiakUser status.phase == Ready")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "riakuser", cbUser,
				"-n", cbNS, "-o", "jsonpath={.status.phase}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("Ready"))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("the requested and issued certificate CN equals the Riak username", func() {
		By("verifying the requested cert-manager Certificate has CommonName == username")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "certificate", clientCertSecret,
				"-n", cbNS, "-o", "jsonpath={.spec.commonName}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal(cbUser))
		}).Should(Succeed())

		By("verifying the issued certificate's Subject CN == username (good for auth)")
		Eventually(func(g Gomega) {
			cn, err := commonNameFromCertSecret(cbNS, clientCertSecret)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cn).To(Equal(cbUser))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("the bucket is reachable and writable over protobuf as the cert user", func() {
		projectDir, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		script := filepath.Join(projectDir, "test", "e2e", "scripts", "verify-cert-bucket.sh")

		By("running the protobuf write/read check with the client certificate")
		Eventually(func(g Gomega) {
			cmd := exec.Command("bash", script,
				cbNS, cbCluster, cbBucketType, cbBucketName, clientCertSecret, cbUser)
			out, err := utils.Run(cmd)
			_, _ = fmt.Fprintf(GinkgoWriter, "verify-cert-bucket.sh output:\n%s\n", out)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("WRITE_OK"))
			g.Expect(out).To(ContainSubstring("PASS"))
		}, 3*time.Minute, 15*time.Second).Should(Succeed())
	})
})

// commonNameFromCertSecret reads a cert-manager-issued TLS Secret and returns the
// Subject CommonName of the leaf certificate in tls.crt.
func commonNameFromCertSecret(namespace, secretName string) (string, error) {
	out, err := utils.Run(exec.Command("kubectl", "get", "secret", secretName,
		"-n", namespace, "-o", `jsonpath={.data.tls\.crt}`))
	if err != nil {
		return "", err
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return "", fmt.Errorf("decoding base64 tls.crt: %w", err)
	}
	block, _ := pem.Decode(der)
	if block == nil {
		return "", fmt.Errorf("no PEM block found in tls.crt")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing certificate: %w", err)
	}
	return cert.Subject.CommonName, nil
}

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
