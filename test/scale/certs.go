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

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os/exec"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

// The two ways a RiakUser gets its client certificate, exercised together:
//
//   - cert-manager: spec.certificateRef.issuerRef; the operator creates a
//     Certificate from the scale Issuer, whose CA is the cluster's own CA.
//   - external CA: spec.certificateRef.externalSecretName; the harness plays the
//     external PKI: it makes its own CA, publishes the CA certificate in a Secret
//     the clusters trust (spec.tls.additionalClientCAs), and issues each such
//     user's certificate itself. The operator creates no Certificate for these.
const (
	externalCASecret = "scale-ext-ca"
	riakCAPath       = "/etc/riak/certs/ca.crt" // Riak's ssl.cacertfile inside the pod
)

// extCA is the harness's stand-in for an external certificate authority.
type extCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newExternalCA() (*extCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "openriak-scale-external-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(7 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &extCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// issueClientCert signs a client-auth certificate whose CommonName is cn (the
// Riak username) and returns it PEM-encoded.
func (ca *extCA) issueClientCert(cn string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// isExternalUser reports whether user index u (0-based, per cluster) takes its
// certificate from the external CA: the last o.externalUsers users do, the rest
// use cert-manager, so both patterns share every cluster.
func isExternalUser(o opts, u int) bool {
	return u >= o.users-o.externalUsers
}

// certsEnabled reports whether clusters run with TLS: the stress clients and the
// external-CA users both need it (client certificates only work over TLS).
func (o opts) certsEnabled() bool { return o.stress || o.externalUsers > 0 }

// clusterTLS is the spec.tls the harness gives every cluster, or nil without TLS.
// The server certificate always comes from the scale CA Issuer; with external
// users the external CA is added as a trusted client CA.
func (o opts) clusterTLS() *riakv1.TLSConfig {
	if !o.certsEnabled() {
		return nil
	}
	tls := &riakv1.TLSConfig{
		Enabled:     true,
		CertManager: &riakv1.CertManagerConfig{IssuerName: "scale-issuer", IssuerKind: "Issuer"},
	}
	if o.externalUsers > 0 {
		tls.AdditionalClientCAs = []riakv1.TrustedCASource{{SecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: externalCASecret}, Key: "ca.crt"}}}
	}
	return tls
}

// userCertRef is the certificateRef for user index u: external Secret or
// cert-manager Issuer.
func userCertRef(o opts, riakUserName string, u int) *riakv1.UserCertificateRef {
	if isExternalUser(o, u) {
		return &riakv1.UserCertificateRef{ExternalSecretName: externalCertSecret(riakUserName)}
	}
	return &riakv1.UserCertificateRef{IssuerRef: &riakv1.CertIssuerRef{Name: "scale-issuer", Kind: "Issuer"}}
}

func externalCertSecret(riakUserName string) string { return riakUserName + "-ext-cert" }

// setupExternalCA creates the external CA and publishes its certificate (never
// its key) in the Secret the clusters trust.
func setupExternalCA(ctx context.Context, c client.Client, o opts) (*extCA, error) {
	ca, err := newExternalCA()
	if err != nil {
		return nil, fmt.Errorf("create external CA: %w", err)
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: externalCASecret, Namespace: o.namespace},
		Data:       map[string][]byte{"ca.crt": ca.pem},
	}
	if err := c.Create(ctx, s); err != nil && !apiAlreadyExists(err) {
		return nil, fmt.Errorf("create %s: %w", externalCASecret, err)
	}
	return ca, nil
}

// createExternalCert issues the user's certificate from the external CA and
// stores it where the RiakUser points.
func createExternalCert(ctx context.Context, c client.Client, o opts, riakUserName, username string) error {
	leaf, err := o.extCA.issueClientCert(username)
	if err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: externalCertSecret(riakUserName), Namespace: o.namespace},
		Data:       map[string][]byte{"tls.crt": leaf},
	}
	if err := c.Create(ctx, s); err != nil && !apiAlreadyExists(err) {
		return fmt.Errorf("create %s: %w", s.Name, err)
	}
	return nil
}

// leafProblem checks one user's certificate the way Riak would against the
// CA file a node actually has: CommonName == username, client-auth usage, valid
// now, chained to a CA in caPEM. "" means fine.
func leafProblem(caPEM, leafPEM []byte, username string, now time.Time) string {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return "the node's ca.crt holds no certificate"
	}
	var certs []*x509.Certificate
	rest := leafPEM
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return "unparseable certificate: " + err.Error()
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return "no certificate in tls.crt"
	}
	if certs[0].Subject.CommonName != username {
		return fmt.Sprintf("CommonName %q, want %q", certs[0].Subject.CommonName, username)
	}
	inter := x509.NewCertPool()
	for _, ic := range certs[1:] {
		inter.AddCert(ic)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return "does not verify against the node's ca.crt: " + err.Error()
	}
	return ""
}

// countCerts returns how many CERTIFICATE blocks pemData holds.
func countCerts(pemData []byte) int {
	n := 0
	for {
		var b *pem.Block
		b, pemData = pem.Decode(pemData)
		if b == nil {
			return n
		}
		if b.Type == "CERTIFICATE" {
			n++
		}
	}
}

// readPodFile is podFile, replaceable in tests.
var readPodFile = podFile

// podFile reads a file from the Riak container.
func podFile(ns, pod, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "exec", "-n", ns, pod, "-c", "riak", "--", "cat", path).Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl exec %s cat %s: %w", pod, path, err)
	}
	return out, nil
}

var certManagerCertificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// userLeafSecret returns the Secret name holding the user's certificate and
// whether the operator should have created a cert-manager Certificate for it.
func userLeafSecret(u *riakv1.RiakUser) (secret string, certManager bool) {
	ref := u.Spec.CertificateRef
	if ref.IssuerRef == nil {
		return ref.ExternalSecretName, false
	}
	if ref.SecretName != "" {
		return ref.SecretName, true
	}
	return u.Name + "-client-tls", true
}

// verifyCerts checks both certificate patterns on every cluster:
//
//   - status: every user reports certificateReady; clusters report no trust bundle
//     error, and with external users trust the cluster CA plus the external CA;
//   - objects: a cert-manager user has a Certificate, an external user has none;
//   - on every node: Riak's real ca.crt holds the expected CAs, and each user's
//     certificate (cert-manager issued or external) verifies against it, which is
//     what lets Riak accept that user's TLS handshake.
func verifyCerts(ctx context.Context, c client.Client, o opts) ([]string, error) {
	clusters := &riakv1.RiakClusterList{}
	users := &riakv1.RiakUserList{}
	for _, l := range []client.ObjectList{clusters, users} {
		if err := c.List(ctx, l, client.InNamespace(o.namespace)); err != nil {
			return nil, err
		}
	}
	var (
		mu       sync.Mutex
		problems []string
		wg       sync.WaitGroup
		sem      = make(chan struct{}, o.verifyWorkers)
	)
	fail := func(format string, a ...interface{}) {
		mu.Lock()
		problems = append(problems, fmt.Sprintf(format, a...))
		mu.Unlock()
	}
	wantCAs := 1
	if o.externalUsers > 0 {
		wantCAs = 2
	}

	for _, cl := range clusters.Items {
		ts := cl.Status.TLSStatus
		if !ts.Enabled {
			fail("%s: TLS is not enabled", cl.Name)
		}
		if ts.TrustBundleError != "" {
			fail("%s: trust bundle error: %s", cl.Name, ts.TrustBundleError)
		}
		if int(ts.TrustedClientCAs) != wantCAs {
			fail("%s: trusts %d client CAs, want %d", cl.Name, ts.TrustedClientCAs, wantCAs)
		}

		var cu []riakv1.RiakUser
		for _, u := range users.Items {
			if u.Spec.ClusterName == cl.Name {
				cu = append(cu, u)
			}
		}
		leafs := map[string][]byte{} // username -> tls.crt
		for i := range cu {
			u := &cu[i]
			if !u.Status.CertificateReady {
				fail("%s: certificate not ready: %s", u.Name, u.Status.CertificateError)
			}
			secret, viaCertManager := userLeafSecret(u)
			cert := &unstructured.Unstructured{}
			cert.SetGroupVersionKind(certManagerCertificateGVK)
			err := c.Get(ctx, client.ObjectKey{Namespace: u.Namespace, Name: u.Name + "-client-tls"}, cert)
			switch {
			case viaCertManager && err != nil:
				fail("%s: cert-manager user has no Certificate: %v", u.Name, err)
			case !viaCertManager && err == nil:
				fail("%s: external-CA user must not get a Certificate", u.Name)
			case !viaCertManager && !apierrors.IsNotFound(err):
				fail("%s: checking for a Certificate: %v", u.Name, err)
			}
			s := &corev1.Secret{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: u.Namespace, Name: secret}, s); err != nil {
				fail("%s: certificate Secret %s: %v", u.Name, secret, err)
				continue
			}
			leafs[u.Spec.Username] = s.Data["tls.crt"]
		}

		for i := int32(0); i < cl.Spec.Size; i++ {
			pod := fmt.Sprintf("%s-%d", cl.Name, i)
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				ca, err := readPodFile(o.namespace, pod, riakCAPath)
				if err != nil {
					fail("%v", err)
					return
				}
				if n := countCerts(ca); n != wantCAs {
					fail("%s: ca.crt holds %d certificates, want %d", pod, n, wantCAs)
				}
				for name, leaf := range leafs {
					if p := leafProblem(ca, leaf, name, time.Now()); p != "" {
						fail("%s: user %s: %s", pod, name, p)
					}
				}
			}()
		}
	}
	wg.Wait()
	sort.Strings(problems)
	return problems, nil
}

// verifyCertsEventually retries verifyCerts: cert-manager issues asynchronously
// and the kubelet refreshes the mounted trust bundle on its own schedule.
func verifyCertsEventually(ctx context.Context, c client.Client, o opts) error {
	fmt.Println("\n── verifying client certificates (cert-manager and external CA) ──")
	deadline := time.Now().Add(o.verifyTimeout)
	for {
		problems, err := verifyCerts(ctx, c, o)
		if err != nil {
			return err
		}
		if len(problems) == 0 {
			fmt.Printf("CERTS OK: every user's certificate is ready and verifies against every node's ca.crt "+
				"(%d external-CA users per cluster, the rest cert-manager)\n", o.externalUsers)
			return nil
		}
		if time.Now().After(deadline) {
			for i, p := range problems {
				if i == 25 {
					fmt.Printf("  ... and %d more\n", len(problems)-25)
					break
				}
				fmt.Println("  CERTS:", p)
			}
			return fmt.Errorf("%d certificate problems", len(problems))
		}
		fmt.Printf("  %d certificate problems, retrying...\n", len(problems))
		time.Sleep(10 * time.Second)
	}
}
