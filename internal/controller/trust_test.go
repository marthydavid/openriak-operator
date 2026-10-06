package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

// fataler is the part of testing.T the cert helpers need, so they also work
// with Ginkgo's GinkgoT().
type fataler interface {
	Helper()
	Fatal(args ...any)
}

// testCA is a throwaway CA for the trust tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t fataler, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{cert: c, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a leaf for cn with the given usages and validity window.
func (ca *testCA) issue(t fataler, cn string, usages []x509.ExtKeyUsage, notBefore, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func (ca *testCA) clientCert(t fataler, cn string) []byte {
	t.Helper()
	return ca.issue(t, cn, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
}

func poolOf(ca ...*testCA) *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range ca {
		p.AddCert(c.cert)
	}
	return p
}

func TestMergeCABundle(t *testing.T) {
	a, b := newTestCA(t, "a"), newTestCA(t, "b")

	bundle, n, err := mergeCABundle(a.pem, append(append([]byte{}, b.pem...), a.pem...))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 distinct certs (a duplicated), got %d", n)
	}
	certs, _ := parsePEMCerts(bundle)
	if len(certs) != 2 || !certs[0].Equal(a.cert) || !certs[1].Equal(b.cert) {
		t.Fatalf("bundle must keep input order a, b: %v", certs)
	}

	// Non-certificate PEM blocks (e.g. a private key) are skipped, not copied.
	key := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("x")})
	bundle, n, _ = mergeCABundle(key, a.pem)
	if n != 1 || strings.Contains(string(bundle), "PRIVATE KEY") {
		t.Fatalf("private key block must not enter the bundle: n=%d", n)
	}

	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("garbage")})
	if _, _, err := mergeCABundle(bad); err == nil {
		t.Fatal("garbage certificate must error")
	}
}

func TestVerifyClientCert(t *testing.T) {
	ca, other := newTestCA(t, "ext-ca"), newTestCA(t, "other-ca")
	now := time.Now()
	good := ca.clientCert(t, "alice")

	exp, err := verifyClientCert(good, "alice", poolOf(ca), now)
	if err != nil || exp.IsZero() {
		t.Fatalf("valid cert rejected: %v", err)
	}

	cases := []struct {
		name string
		pem  []byte
		user string
		pool *x509.CertPool
		want string
	}{
		{"wrong CN", good, "bob", poolOf(ca), "does not match spec.username"},
		{"untrusted CA", good, "alice", poolOf(other), "trusted CA"},
		{"expired", ca.issue(t, "alice", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			now.Add(-48*time.Hour), now.Add(-24*time.Hour)), "alice", poolOf(ca), "expired"},
		{"server-only usage", ca.issue(t, "alice", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			now.Add(-time.Minute), now.Add(time.Hour)), "alice", poolOf(ca), "trusted CA"},
		{"no PEM", []byte("nope"), "alice", poolOf(ca), "no PEM CERTIFICATE"},
	}
	for _, tc := range cases {
		_, err := verifyClientCert(tc.pem, tc.user, tc.pool, now)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func trustScheme(t *testing.T) *runtime.Scheme {
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

func tlsCluster(extra ...riakv1.TrustedCASource) *riakv1.RiakCluster {
	return &riakv1.RiakCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
		Spec: riakv1.RiakClusterSpec{TLS: &riakv1.TLSConfig{
			Enabled:             true,
			CertManager:         &riakv1.CertManagerConfig{IssuerName: "i"},
			AdditionalClientCAs: extra,
		}},
	}
}

func secretKey(name, key string) riakv1.TrustedCASource {
	return riakv1.TrustedCASource{SecretRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
}

func cmKey(name, key string) riakv1.TrustedCASource {
	return riakv1.TrustedCASource{ConfigMapRef: &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
}

func TestBuildTrustBundle(t *testing.T) {
	clusterCA, ext, ext2 := newTestCA(t, "cluster"), newTestCA(t, "ext"), newTestCA(t, "ext2")
	ctx := context.Background()
	objs := []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "c-tls", Namespace: "ns"},
			Data: map[string][]byte{"ca.crt": clusterCA.pem}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: "ns"},
			Data: map[string][]byte{"ca.crt": ext.pem, "other": []byte("x"), "junk": []byte("not pem")}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "bundle", Namespace: "ns"},
			Data: map[string]string{"ca.pem": string(ext2.pem) + string(ext.pem)}},
	}
	c := fake.NewClientBuilder().WithScheme(trustScheme(t)).WithObjects(objs...).Build()

	t.Run("cluster CA then extras from a Secret and a ConfigMap, deduplicated", func(t *testing.T) {
		cl := tlsCluster(secretKey("ext", "ca.crt"), cmKey("bundle", "ca.pem"))
		bundle, n, missing, err := buildTrustBundle(ctx, c, cl)
		if err != nil || missing || n != 3 {
			t.Fatalf("n=%d missing=%v err=%v", n, missing, err)
		}
		certs, _ := parsePEMCerts(bundle)
		if !certs[0].Equal(clusterCA.cert) || !certs[1].Equal(ext.cert) || !certs[2].Equal(ext2.cert) {
			t.Fatal("unexpected bundle order")
		}
	})

	t.Run("cluster CA not issued yet", func(t *testing.T) {
		cl := tlsCluster(secretKey("ext", "ca.crt"))
		cl.Name = "fresh"
		_, n, missing, err := buildTrustBundle(ctx, c, cl)
		if err != nil || !missing || n != 1 {
			t.Fatalf("n=%d missing=%v err=%v", n, missing, err)
		}
	})

	for name, tc := range map[string]struct {
		src  riakv1.TrustedCASource
		want string
	}{
		"missing Secret":    {secretKey("nope", "ca.crt"), `Secret "nope" not found`},
		"missing key":       {secretKey("ext", "absent"), `has no key "absent"`},
		"missing ConfigMap": {cmKey("nope", "k"), `ConfigMap "nope" not found`},
		"no PEM":            {secretKey("ext", "junk"), "holds no PEM CERTIFICATE"},
		"empty entry":       {riakv1.TrustedCASource{}, "neither secretRef nor configMapRef"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := buildTrustBundle(ctx, c, tlsCluster(tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}

	t.Run("cluster Secret without ca.crt", func(t *testing.T) {
		c2 := fake.NewClientBuilder().WithScheme(trustScheme(t)).WithObjects(
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "c-tls", Namespace: "ns"}}).Build()
		if _, _, _, err := buildTrustBundle(ctx, c2, tlsCluster()); err == nil ||
			!strings.Contains(err.Error(), "no ca.crt") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("no TLS means no trust", func(t *testing.T) {
		cl := tlsCluster(secretKey("ext", "ca.crt"))
		cl.Spec.TLS.Enabled = false
		bundle, n, _, err := buildTrustBundle(ctx, c, cl)
		if err != nil || n != 0 || len(bundle) != 0 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if hasAdditionalClientCAs(cl) {
			t.Fatal("additional CAs only apply with TLS enabled")
		}
	})
}

func TestExternalCertificateReadiness(t *testing.T) {
	clusterCA, ext, rogue := newTestCA(t, "cluster"), newTestCA(t, "ext"), newTestCA(t, "rogue")
	ctx := context.Background()
	now := time.Now()
	cl := tlsCluster(secretKey("ext", "ca.crt"))
	user := func(secret string) *riakv1.RiakUser {
		return &riakv1.RiakUser{
			ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "ns"},
			Spec: riakv1.RiakUserSpec{Username: "alice",
				CertificateRef: &riakv1.UserCertificateRef{ExternalSecretName: secret}},
		}
	}
	mk := func(name string, data map[string][]byte) client.Object {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}, Data: data}
	}
	c := fake.NewClientBuilder().WithScheme(trustScheme(t)).WithObjects(
		mk("c-tls", map[string][]byte{"ca.crt": clusterCA.pem}),
		mk("ext", map[string][]byte{"ca.crt": ext.pem}),
		mk("good", map[string][]byte{"tls.crt": ext.clientCert(t, "alice")}),
		mk("by-cluster-ca", map[string][]byte{"tls.crt": clusterCA.clientCert(t, "alice")}),
		mk("wrong-ca", map[string][]byte{"tls.crt": rogue.clientCert(t, "alice")}),
		mk("wrong-cn", map[string][]byte{"tls.crt": ext.clientCert(t, "bob")}),
		mk("no-crt", map[string][]byte{"other": []byte("x")}),
	).Build()

	for _, tc := range []struct {
		secret string
		ready  bool
		want   string
	}{
		{"good", true, ""},
		{"by-cluster-ca", true, ""}, // the cluster's own CA stays trusted
		{"wrong-ca", false, "trusted CA"},
		{"wrong-cn", false, "does not match spec.username"},
		{"no-crt", false, "no tls.crt"},
		{"absent", false, "does not exist"},
	} {
		ready, reason, notAfter := externalCertificateReadiness(ctx, c, user(tc.secret), cl, now)
		if ready != tc.ready || !strings.Contains(reason, tc.want) {
			t.Errorf("%s: ready=%v reason=%q", tc.secret, ready, reason)
		}
		if tc.ready && notAfter.IsZero() {
			t.Errorf("%s: expiry must be reported", tc.secret)
		}
	}

	t.Run("cluster without TLS trusts no CA", func(t *testing.T) {
		noTLS := &riakv1.RiakCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}}
		ready, reason, _ := externalCertificateReadiness(ctx, c, user("good"), noTLS, now)
		if ready || !strings.Contains(reason, "trusts no CA") {
			t.Fatalf("ready=%v reason=%q", ready, reason)
		}
	})
}

func TestCertRequeue(t *testing.T) {
	ext := &riakv1.RiakUser{Spec: riakv1.RiakUserSpec{CertificateRef: &riakv1.UserCertificateRef{ExternalSecretName: "s"}}}
	cm := &riakv1.RiakUser{Spec: riakv1.RiakUserSpec{CertificateRef: &riakv1.UserCertificateRef{
		IssuerRef: &riakv1.CertIssuerRef{Name: "i"}}}}

	if r := certRequeue(cm, false, time.Time{}); r.RequeueAfter != 30*time.Second {
		t.Errorf("pending: %v", r.RequeueAfter)
	}
	if r := certRequeue(cm, true, time.Time{}); r.RequeueAfter != 0 {
		t.Errorf("cert-manager cert that is Ready needs no requeue: %v", r.RequeueAfter)
	}
	if r := certRequeue(ext, true, time.Now().Add(48*time.Hour)); r.RequeueAfter != externalCertRecheck {
		t.Errorf("external, far from expiry: %v", r.RequeueAfter)
	}
	r := certRequeue(ext, true, time.Now().Add(2*time.Minute))
	if r.RequeueAfter <= 0 || r.RequeueAfter > 2*time.Minute {
		t.Errorf("external, about to expire must re-check by then: %v", r.RequeueAfter)
	}
}

func TestTLSVolume(t *testing.T) {
	plain := tlsVolume(tlsCluster())
	if plain.Secret == nil || plain.Secret.SecretName != "c-tls" || plain.Projected != nil {
		t.Fatalf("without extra CAs the cert-manager Secret is mounted as is: %+v", plain)
	}
	v := tlsVolume(tlsCluster(secretKey("ext", "ca.crt")))
	if v.Projected == nil || len(v.Projected.Sources) != 2 {
		t.Fatalf("want a projected volume of two Secrets: %+v", v)
	}
	if got := v.Projected.Sources[1].Secret; got.Name != "c-tls-trust" || got.Items[0].Path != "ca.crt" {
		t.Fatalf("ca.crt must come from the trust bundle: %+v", got)
	}
}
