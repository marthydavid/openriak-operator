package main

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

func certScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := riakv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func mustCA(t *testing.T) *extCA {
	t.Helper()
	ca, err := newExternalCA()
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func mustLeaf(t *testing.T, ca *extCA, cn string) []byte {
	t.Helper()
	leaf, err := ca.issueClientCert(cn)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestLeafProblem(t *testing.T) {
	ca, other := mustCA(t), mustCA(t)
	leaf := mustLeaf(t, ca, "alice")
	now := time.Now()

	if p := leafProblem(ca.pem, leaf, "alice", now); p != "" {
		t.Fatalf("valid certificate rejected: %s", p)
	}
	// A node's ca.crt may hold several CAs (cluster CA + external CA).
	both := append(append([]byte{}, other.pem...), ca.pem...)
	if p := leafProblem(both, leaf, "alice", now); p != "" {
		t.Fatalf("certificate from the second CA in the bundle rejected: %s", p)
	}
	for name, tc := range map[string]struct {
		ca, leaf []byte
		user     string
		now      time.Time
		want     string
	}{
		"wrong CN":     {ca.pem, leaf, "bob", now, "CommonName"},
		"untrusted CA": {other.pem, leaf, "alice", now, "does not verify"},
		"expired":      {ca.pem, leaf, "alice", now.Add(72 * time.Hour), "does not verify"},
		"empty ca.crt": {[]byte("nope"), leaf, "alice", now, "holds no certificate"},
		"no leaf":      {ca.pem, []byte("nope"), "alice", now, "no certificate"},
	} {
		if p := leafProblem(tc.ca, tc.leaf, tc.user, tc.now); !strings.Contains(p, tc.want) {
			t.Errorf("%s: got %q, want it to contain %q", name, p, tc.want)
		}
	}
	if countCerts(both) != 2 || countCerts(nil) != 0 {
		t.Fatal("countCerts")
	}
}

func TestCertPatternAssignment(t *testing.T) {
	o := opts{users: 4, externalUsers: 2}
	var ext []int
	for u := 0; u < o.users; u++ {
		ref := userCertRef(o, "c-u", u)
		switch {
		case isExternalUser(o, u):
			ext = append(ext, u)
			if ref.IssuerRef != nil || ref.ExternalSecretName != "c-u-ext-cert" {
				t.Errorf("user %d should be external: %+v", u, ref)
			}
		default:
			if ref.IssuerRef == nil || ref.IssuerRef.Name != "scale-issuer" || ref.ExternalSecretName != "" {
				t.Errorf("user %d should use cert-manager: %+v", u, ref)
			}
		}
	}
	if len(ext) != 2 || ext[0] != 2 || ext[1] != 3 {
		t.Fatalf("the last two users are external, got %v", ext)
	}

	if (opts{users: 4}).clusterTLS() != nil {
		t.Fatal("no TLS unless stress or external users")
	}
	if tls := (opts{users: 4, stress: true}).clusterTLS(); tls == nil || len(tls.AdditionalClientCAs) != 0 {
		t.Fatalf("stress: TLS without extra CAs, got %+v", tls)
	}
	tls := o.clusterTLS()
	if tls == nil || !tls.Enabled || len(tls.AdditionalClientCAs) != 1 ||
		tls.AdditionalClientCAs[0].SecretRef.Name != externalCASecret {
		t.Fatalf("external users: TLS trusting the external CA, got %+v", tls)
	}
}

func TestValidateOpts_externalUsers(t *testing.T) {
	ok := opts{ringSize: minRingSize, users: 3, externalUsers: 3}
	if err := validateOpts(ok); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{-1, 4} {
		bad := ok
		bad.externalUsers = n
		if err := validateOpts(bad); err == nil || !strings.Contains(err.Error(), "-external-users") {
			t.Errorf("externalUsers=%d: got %v", n, err)
		}
	}
}

func TestUserLeafSecret(t *testing.T) {
	mk := func(ref *riakv1.UserCertificateRef) *riakv1.RiakUser {
		return &riakv1.RiakUser{ObjectMeta: metav1.ObjectMeta{Name: "u"}, Spec: riakv1.RiakUserSpec{CertificateRef: ref}}
	}
	if s, cm := userLeafSecret(mk(&riakv1.UserCertificateRef{ExternalSecretName: "ext"})); s != "ext" || cm {
		t.Errorf("external: %s %v", s, cm)
	}
	issuer := &riakv1.CertIssuerRef{Name: "i"}
	if s, cm := userLeafSecret(mk(&riakv1.UserCertificateRef{IssuerRef: issuer})); s != "u-client-tls" || !cm {
		t.Errorf("default cert-manager Secret: %s %v", s, cm)
	}
	if s, cm := userLeafSecret(mk(&riakv1.UserCertificateRef{IssuerRef: issuer, SecretName: "mine"})); s != "mine" || !cm {
		t.Errorf("custom cert-manager Secret: %s %v", s, cm)
	}
}

// TestCreateAll_bothPatterns runs the real manifest generation against a fake
// client: every cluster must trust the external CA, the cert-manager users must
// point at the Issuer, and each external user needs a Secret whose certificate
// the external CA signed with CN == username.
func TestCreateAll_bothPatterns(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(certScheme(t)).Build()
	o := opts{
		clusters: 2, users: 3, externalUsers: 1, buckets: 1, replicas: 1, ringSize: minRingSize,
		namespace: "ns", extCA: mustCA(t),
	}
	if err := createAll(ctx, c, o); err != nil {
		t.Fatal(err)
	}

	clusters := &riakv1.RiakClusterList{}
	users := &riakv1.RiakUserList{}
	if err := c.List(ctx, clusters); err != nil {
		t.Fatal(err)
	}
	if err := c.List(ctx, users); err != nil {
		t.Fatal(err)
	}
	if len(clusters.Items) != 2 || len(users.Items) != 6 {
		t.Fatalf("clusters=%d users=%d", len(clusters.Items), len(users.Items))
	}
	for _, cl := range clusters.Items {
		if cl.Spec.TLS == nil || !cl.Spec.TLS.Enabled || len(cl.Spec.TLS.AdditionalClientCAs) != 1 {
			t.Fatalf("%s must trust the external CA: %+v", cl.Name, cl.Spec.TLS)
		}
	}
	var nExt, nCM int
	for _, u := range users.Items {
		ref := u.Spec.CertificateRef
		if ref.IssuerRef != nil {
			nCM++
			continue
		}
		nExt++
		s := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: ref.ExternalSecretName}, s); err != nil {
			t.Fatalf("external user %s has no Secret: %v", u.Name, err)
		}
		if p := leafProblem(o.extCA.pem, s.Data["tls.crt"], u.Spec.Username, time.Now()); p != "" {
			t.Fatalf("external user %s: %s", u.Name, p)
		}
		if _, ok := s.Data["tls.key"]; ok {
			t.Fatal("the harness must not store a private key it does not need")
		}
	}
	if nExt != 2 || nCM != 4 {
		t.Fatalf("want 2 external + 4 cert-manager users, got %d + %d", nExt, nCM)
	}
}

func TestSetupExternalCA_publishesOnlyTheCertificate(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(certScheme(t)).Build()
	ca, err := setupExternalCA(ctx, c, opts{namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	s := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: externalCASecret}, s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["ca.crt"]) != string(ca.pem) || len(s.Data) != 1 {
		t.Fatalf("only ca.crt may be published, got keys %v", s.Data)
	}
}

// certFixture builds a namespace with one cluster holding one cert-manager user
// and one external-CA user, the way the operator leaves them after convergence.
type certFixture struct {
	clusterCA, extCA *extCA
	objs             []client.Object
}

func newCertFixture(t *testing.T, mutate func(*certFixture)) *certFixture {
	t.Helper()
	f := &certFixture{clusterCA: mustCA(t), extCA: mustCA(t)}
	cluster := &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec:       riakv1.RiakClusterSpec{Size: 2},
		Status: riakv1.RiakClusterStatus{TLSStatus: riakv1.TLSStatus{
			Enabled: true, TrustedClientCAs: 2}},
	}
	cmUser := &riakv1.RiakUser{
		ObjectMeta: metav1.ObjectMeta{Name: "c-u000", Namespace: "ns"},
		Spec: riakv1.RiakUserSpec{ClusterName: "c", Username: "c_u000", CertificateRef: &riakv1.UserCertificateRef{
			IssuerRef: &riakv1.CertIssuerRef{Name: "scale-issuer"}}},
		Status: riakv1.RiakUserStatus{CertificateReady: true},
	}
	extUser := &riakv1.RiakUser{
		ObjectMeta: metav1.ObjectMeta{Name: "c-u001", Namespace: "ns"},
		Spec: riakv1.RiakUserSpec{ClusterName: "c", Username: "c_u001", CertificateRef: &riakv1.UserCertificateRef{
			ExternalSecretName: "c-u001-ext-cert"}},
		Status: riakv1.RiakUserStatus{CertificateReady: true},
	}
	secret := func(name string, crt []byte) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Data: map[string][]byte{"tls.crt": crt}}
	}
	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(certManagerCertificateGVK)
	certificate.SetName("c-u000-client-tls")
	certificate.SetNamespace("ns")

	f.objs = []client.Object{
		cluster, cmUser, extUser,
		secret("c-u000-client-tls", mustLeaf(t, f.clusterCA, "c_u000")),
		secret("c-u001-ext-cert", mustLeaf(t, f.extCA, "c_u001")),
		certificate,
	}
	if mutate != nil {
		mutate(f)
	}
	return f
}

func runVerifyCerts(t *testing.T, f *certFixture, nodeCA []byte) []string {
	t.Helper()
	old := readPodFile
	readPodFile = func(_, _, _ string) ([]byte, error) { return nodeCA, nil }
	t.Cleanup(func() { readPodFile = old })
	c := fake.NewClientBuilder().WithScheme(certScheme(t)).WithObjects(f.objs...).Build()
	problems, err := verifyCerts(context.Background(), c, opts{namespace: "ns", externalUsers: 1, verifyWorkers: 2})
	if err != nil {
		t.Fatal(err)
	}
	return problems
}

func TestVerifyCerts_bothPatterns(t *testing.T) {
	t.Run("both patterns verify against the node's merged ca.crt", func(t *testing.T) {
		f := newCertFixture(t, nil)
		nodeCA := append(append([]byte{}, f.clusterCA.pem...), f.extCA.pem...)
		if p := runVerifyCerts(t, f, nodeCA); len(p) != 0 {
			t.Fatalf("unexpected problems: %v", p)
		}
	})

	t.Run("a node that only trusts the cluster CA rejects the external user (on every node)", func(t *testing.T) {
		f := newCertFixture(t, nil)
		p := runVerifyCerts(t, f, f.clusterCA.pem)
		joined := strings.Join(p, "\n")
		if !strings.Contains(joined, "c_u001") || !strings.Contains(joined, "ca.crt holds 1 certificates, want 2") {
			t.Fatalf("want the external user and the bundle size reported: %v", p)
		}
		if strings.Contains(joined, "user c_u000") {
			t.Fatalf("the cert-manager user chains to the cluster CA and must pass: %v", p)
		}
	})

	t.Run("an external user must not get a cert-manager Certificate, a cert-manager user must", func(t *testing.T) {
		f := newCertFixture(t, func(f *certFixture) {
			extra := &unstructured.Unstructured{}
			extra.SetGroupVersionKind(certManagerCertificateGVK)
			extra.SetName("c-u001-client-tls")
			extra.SetNamespace("ns")
			f.objs = append(f.objs[:5], extra) // drops the c-u000 Certificate, adds one for the external user
		})
		nodeCA := append(append([]byte{}, f.clusterCA.pem...), f.extCA.pem...)
		joined := strings.Join(runVerifyCerts(t, f, nodeCA), "\n")
		if !strings.Contains(joined, "c-u000: cert-manager user has no Certificate") ||
			!strings.Contains(joined, "c-u001: external-CA user must not get a Certificate") {
			t.Fatalf("got %v", joined)
		}
	})

	t.Run("status problems are reported", func(t *testing.T) {
		f := newCertFixture(t, func(f *certFixture) {
			cl := f.objs[0].(*riakv1.RiakCluster)
			cl.Status.TLSStatus.TrustBundleError = "bad CA"
			cl.Status.TLSStatus.TrustedClientCAs = 1
			f.objs[2].(*riakv1.RiakUser).Status = riakv1.RiakUserStatus{CertificateError: "untrusted"}
		})
		nodeCA := append(append([]byte{}, f.clusterCA.pem...), f.extCA.pem...)
		joined := strings.Join(runVerifyCerts(t, f, nodeCA), "\n")
		for _, want := range []string{"trust bundle error: bad CA", "trusts 1 client CAs, want 2",
			"c-u001: certificate not ready: untrusted"} {
			if !strings.Contains(joined, want) {
				t.Errorf("missing %q in:\n%s", want, joined)
			}
		}
	})
}
