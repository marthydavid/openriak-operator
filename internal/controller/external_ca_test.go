package controller

import (
	"context"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
	"github.com/marthydavid/openriak-operator/internal/riak"
)

var _ = Describe("External client CAs", func() {
	const (
		ns          = "default"
		clusterName = "extca-cluster"
		userName    = "extca-user"
	)
	ctx := context.Background()
	nn := types.NamespacedName{Name: clusterName, Namespace: ns}
	trustNN := types.NamespacedName{Name: clusterName + "-tls-trust", Namespace: ns}

	noopRunner := func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil }
	var clusterCA, extCA *testCA

	secret := func(name string, data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: data}
	}
	reconcileTheCluster := func() {
		_, err := reconcileCluster(ctx, clusterName, ns)
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		clusterCA, extCA = newTestCA(GinkgoT(), "cluster-ca"), newTestCA(GinkgoT(), "external-ca")
		Expect(k8sClient.Create(ctx, secret("extca-roots", map[string][]byte{"ca.crt": extCA.pem}))).To(Succeed())
		Expect(k8sClient.Create(ctx, &riakv1.RiakCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec: riakv1.RiakClusterSpec{
				Size:  1,
				Image: "basho/riak-kv:latest",
				TLS: &riakv1.TLSConfig{
					Enabled:             true,
					CertManager:         &riakv1.CertManagerConfig{IssuerName: "test-issuer"},
					AdditionalClientCAs: []riakv1.TrustedCASource{secretKey("extca-roots", "ca.crt")},
				},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		_ = k8sClient.Delete(ctx, &riakv1.RiakUser{ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns}})
		// Drain the finalizer with a no-op executor: the default one would run kubectl.
		ur := &RiakUserReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
		}
		_, _ = ur.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: userName, Namespace: ns}})
		c := &riakv1.RiakCluster{}
		if err := k8sClient.Get(ctx, nn, c); err == nil {
			_ = k8sClient.Delete(ctx, c)
			_, _ = reconcileCluster(ctx, clusterName, ns)
		}
		for _, n := range []string{"extca-roots", clusterName + "-tls", clusterName + "-tls-trust", "extca-user-cert"} {
			_ = k8sClient.Delete(ctx, secret(n, nil))
		}
	})

	It("rejects an entry that sets neither or both of secretRef/configMapRef", func() {
		c := &riakv1.RiakCluster{}
		Expect(k8sClient.Get(ctx, nn, c)).To(Succeed())
		both := secretKey("a", "k")
		both.ConfigMapRef = cmKey("b", "k").ConfigMapRef
		for _, bad := range []riakv1.TrustedCASource{{}, both} {
			c.Spec.TLS.AdditionalClientCAs = []riakv1.TrustedCASource{bad}
			err := k8sClient.Update(ctx, c)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("exactly one of secretRef or configMapRef"))
		}
	})

	It("waits for the cluster certificate, then writes the merged bundle and mounts it as ca.crt", func() {
		By("no trust Secret before cert-manager has issued the cluster certificate")
		reconcileTheCluster()
		Expect(errors.IsNotFound(k8sClient.Get(ctx, trustNN, &corev1.Secret{}))).To(BeTrue())

		By("the bundle appearing once the cluster Secret exists: cluster CA first, then the external CA")
		Expect(k8sClient.Create(ctx, secret(clusterName+"-tls", map[string][]byte{"ca.crt": clusterCA.pem}))).To(Succeed())
		reconcileTheCluster()
		trust := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, trustNN, trust)).To(Succeed())
		certs, err := parsePEMCerts(trust.Data["ca.crt"])
		Expect(err).NotTo(HaveOccurred())
		Expect(certs).To(HaveLen(2))
		Expect(certs[0].Equal(clusterCA.cert)).To(BeTrue())
		Expect(certs[1].Equal(extCA.cert)).To(BeTrue())

		By("the StatefulSet projecting tls.crt/tls.key from the cert and ca.crt from the bundle")
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
		var vol *corev1.Volume
		for i := range sts.Spec.Template.Spec.Volumes {
			if sts.Spec.Template.Spec.Volumes[i].Name == riakTLSVolumeName {
				vol = &sts.Spec.Template.Spec.Volumes[i]
			}
		}
		Expect(vol).NotTo(BeNil())
		Expect(vol.Projected).NotTo(BeNil())
		Expect(vol.Projected.Sources[1].Secret.Name).To(Equal(clusterName + "-tls-trust"))

		By("status reporting two trusted CAs and no error")
		c := &riakv1.RiakCluster{}
		Expect(k8sClient.Get(ctx, nn, c)).To(Succeed())
		Expect(c.Status.TLSStatus.TrustedClientCAs).To(Equal(int32(2)))
		Expect(c.Status.TLSStatus.TrustBundleError).To(BeEmpty())

		By("an unchanged bundle not being rewritten, and a CA change being picked up")
		rv := trust.ResourceVersion
		reconcileTheCluster()
		Expect(k8sClient.Get(ctx, trustNN, trust)).To(Succeed())
		Expect(trust.ResourceVersion).To(Equal(rv))

		newCA := newTestCA(GinkgoT(), "rotated-ca")
		roots := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "extca-roots", Namespace: ns}, roots)).To(Succeed())
		roots.Data["ca.crt"] = newCA.pem
		Expect(k8sClient.Update(ctx, roots)).To(Succeed())
		reconcileTheCluster()
		Expect(k8sClient.Get(ctx, trustNN, trust)).To(Succeed())
		certs, _ = parsePEMCerts(trust.Data["ca.crt"])
		Expect(certs).To(HaveLen(2))
		Expect(certs[1].Equal(newCA.cert)).To(BeTrue())
	})

	It("reports a bad CA source in status instead of failing the reconcile", func() {
		Expect(k8sClient.Create(ctx, secret(clusterName+"-tls", map[string][]byte{"ca.crt": clusterCA.pem}))).To(Succeed())
		c := &riakv1.RiakCluster{}
		Expect(k8sClient.Get(ctx, nn, c)).To(Succeed())
		c.Spec.TLS.AdditionalClientCAs = []riakv1.TrustedCASource{secretKey("extca-roots", "absent")}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())

		reconcileTheCluster()

		Expect(k8sClient.Get(ctx, nn, c)).To(Succeed())
		Expect(c.Status.TLSStatus.TrustBundleError).To(ContainSubstring(`has no key "absent"`))
		Expect(errors.IsNotFound(k8sClient.Get(ctx, trustNN, &corev1.Secret{}))).To(BeTrue())
	})

	Context("a RiakUser with an externally issued certificate", func() {
		mkUser := func() {
			Expect(k8sClient.Create(ctx, &riakv1.RiakUser{
				ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: ns},
				Spec: riakv1.RiakUserSpec{
					ClusterName: clusterName,
					Username:    "alice",
					CertificateRef: &riakv1.UserCertificateRef{
						ExternalSecretName: "extca-user-cert",
					},
				},
			})).To(Succeed())
		}
		readyCluster := func() {
			Expect(k8sClient.Create(ctx, secret(clusterName+"-tls", map[string][]byte{"ca.crt": clusterCA.pem}))).To(Succeed())
			reconcileTheCluster()
			c := &riakv1.RiakCluster{}
			Expect(k8sClient.Get(ctx, nn, c)).To(Succeed())
			c.Status.Phase = riakv1.PhaseReady
			c.Status.SecurityEnabled = true
			c.Status.Members = []riakv1.RiakNodeMember{{Pod: clusterName + "-0", Name: clusterName + "-0"}}
			Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
		}
		reconcileTheUser := func() reconcile.Result {
			r := &RiakUserReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
			}
			var res reconcile.Result
			for i := 0; i < 2; i++ { // first pass adds the finalizer
				var err error
				res, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: userName, Namespace: ns}})
				Expect(err).NotTo(HaveOccurred())
			}
			return res
		}
		get := func() *riakv1.RiakUser {
			u := &riakv1.RiakUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: userName, Namespace: ns}, u)).To(Succeed())
			return u
		}

		It("rejects issuerRef together with externalSecretName, or neither", func() {
			for _, ref := range []*riakv1.UserCertificateRef{
				{},
				{IssuerRef: &riakv1.CertIssuerRef{Name: "i"}, ExternalSecretName: "s"},
				{ExternalSecretName: "s", SecretName: "other"},
			} {
				err := k8sClient.Create(ctx, &riakv1.RiakUser{
					ObjectMeta: metav1.ObjectMeta{Name: "extca-invalid", Namespace: ns},
					Spec:       riakv1.RiakUserSpec{ClusterName: clusterName, Username: "x", CertificateRef: ref},
				})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Or(ContainSubstring("exactly one of issuerRef"), ContainSubstring("secretName only applies")))
			}
		})

		It("is Ready with certificateReady once the Secret holds a certificate from the external CA, and creates no Certificate", func() {
			readyCluster()
			Expect(k8sClient.Create(ctx, secret("extca-user-cert", map[string][]byte{"tls.crt": extCA.clientCert(GinkgoT(), "alice")}))).To(Succeed())
			mkUser()

			res := reconcileTheUser()

			u := get()
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseReady), u.Status.Error)
			Expect(u.Status.CertificateReady).To(BeTrue())
			Expect(u.Status.CertificateError).To(BeEmpty())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0), "an external certificate is re-validated periodically")
			cert := &unstructured.Unstructured{}
			cert.SetGroupVersionKind(certificateGVK)
			Expect(errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: userCertName(userName), Namespace: ns},
				cert))).To(BeTrue(), "no cert-manager Certificate for an external certificate")
		})

		It("reports the problem when the certificate is not from a trusted CA", func() {
			readyCluster()
			rogue := newTestCA(GinkgoT(), "rogue")
			Expect(k8sClient.Create(ctx, secret("extca-user-cert", map[string][]byte{"tls.crt": rogue.clientCert(GinkgoT(), "alice")}))).To(Succeed())
			mkUser()

			res := reconcileTheUser()

			u := get()
			Expect(u.Status.Phase).To(Equal(riakv1.UserPhaseReady), "the Riak identity is still provisioned: "+u.Status.Error)
			Expect(u.Status.CertificateReady).To(BeFalse())
			Expect(u.Status.CertificateError).To(ContainSubstring("trusted CA"))
			Expect(res.RequeueAfter.Seconds()).To(BeNumerically("==", 30))
		})

		It("reports a missing Secret and recovers once it appears", func() {
			readyCluster()
			mkUser()
			reconcileTheUser()
			Expect(get().Status.CertificateError).To(ContainSubstring(`Secret "extca-user-cert" does not exist`))

			Expect(k8sClient.Create(ctx, secret("extca-user-cert", map[string][]byte{"tls.crt": extCA.clientCert(GinkgoT(), "alice")}))).To(Succeed())
			reconcileTheUser()
			Expect(get().Status.CertificateReady).To(BeTrue())
		})
	})
})
