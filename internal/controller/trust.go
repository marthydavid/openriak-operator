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

package controller

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	riakv1 "github.com/marthydavid/openriak-operator/api/v1"
)

const (
	// trustBundleKey is the key of the merged CA bundle, mounted as ca.crt.
	trustBundleKey = "ca.crt"

	// externalCertRecheck is how often a valid externally issued user certificate
	// is re-validated, so rotation or expiry shows up in the status.
	externalCertRecheck = 10 * time.Minute
)

// clusterTrustSecretName is the operator-owned Secret holding the merged CA
// bundle Riak uses as ssl.cacertfile when spec.tls.additionalClientCAs is set.
func clusterTrustSecretName(clusterName string) string {
	return clusterName + "-tls-trust"
}

// hasAdditionalClientCAs reports whether the cluster trusts CAs beyond its own.
func hasAdditionalClientCAs(cluster *riakv1.RiakCluster) bool {
	return cluster.Spec.TLS != nil && cluster.Spec.TLS.Enabled && len(cluster.Spec.TLS.AdditionalClientCAs) > 0
}

// reader returns the uncached API reader when one is set, else the (cached)
// client. Secrets are read uncached on purpose: a cached Get would start an
// informer over every Secret in the cluster.
func reader(apiReader client.Reader, c client.Client) client.Reader {
	if apiReader != nil {
		return apiReader
	}
	return c
}

// parsePEMCerts returns every CERTIFICATE block of data as a parsed certificate.
func parsePEMCerts(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid certificate: %w", err)
		}
		certs = append(certs, c)
	}
	return certs, nil
}

// mergeCABundle concatenates the CA certificates of every PEM input into one
// bundle, in input order, dropping duplicates. It returns the bundle and the
// number of distinct certificates.
func mergeCABundle(inputs ...[]byte) ([]byte, int, error) {
	var out bytes.Buffer
	seen := map[string]bool{}
	n := 0
	for _, in := range inputs {
		certs, err := parsePEMCerts(in)
		if err != nil {
			return nil, 0, err
		}
		for _, c := range certs {
			if seen[string(c.Raw)] {
				continue
			}
			seen[string(c.Raw)] = true
			n++
			if err := pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}); err != nil {
				return nil, 0, err
			}
		}
	}
	return out.Bytes(), n, nil
}

// readTrustedCA returns the PEM held by one additionalClientCAs entry.
func readTrustedCA(ctx context.Context, r client.Reader, ns string, src riakv1.TrustedCASource) ([]byte, error) {
	switch {
	case src.SecretRef != nil:
		ref := src.SecretRef
		s := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, s); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("additionalClientCAs: Secret %q not found", ref.Name)
			}
			return nil, err
		}
		v, ok := s.Data[ref.Key]
		if !ok {
			return nil, fmt.Errorf("additionalClientCAs: Secret %q has no key %q", ref.Name, ref.Key)
		}
		return v, nil
	case src.ConfigMapRef != nil:
		ref := src.ConfigMapRef
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("additionalClientCAs: ConfigMap %q not found", ref.Name)
			}
			return nil, err
		}
		v, ok := cm.Data[ref.Key]
		if !ok {
			return nil, fmt.Errorf("additionalClientCAs: ConfigMap %q has no key %q", ref.Name, ref.Key)
		}
		return []byte(v), nil
	}
	return nil, fmt.Errorf("additionalClientCAs: entry sets neither secretRef nor configMapRef")
}

// buildTrustBundle returns the CA bundle Riak must trust for client certificates:
// the cluster's own CA (ca.crt of the cluster TLS Secret, when TLS is enabled and
// the Secret exists) followed by every spec.tls.additionalClientCAs entry, with
// duplicates removed. clusterCAMissing is true when the cluster certificate has
// not been issued yet; the bundle then holds only the additional CAs.
func buildTrustBundle(ctx context.Context, r client.Reader, cluster *riakv1.RiakCluster) (
	bundle []byte, count int, clusterCAMissing bool, err error,
) {
	var inputs [][]byte
	if cluster.Spec.TLS != nil && cluster.Spec.TLS.Enabled {
		s := &corev1.Secret{}
		key := client.ObjectKey{Namespace: cluster.Namespace, Name: clusterTLSSecretName(cluster.Name)}
		switch gerr := r.Get(ctx, key, s); {
		case apierrors.IsNotFound(gerr):
			clusterCAMissing = true
		case gerr != nil:
			return nil, 0, false, gerr
		default:
			ca, ok := s.Data["ca.crt"]
			if !ok {
				return nil, 0, false, fmt.Errorf("cluster TLS Secret %q has no ca.crt key; Riak needs the issuing CA there", key.Name)
			}
			inputs = append(inputs, ca)
		}
		for _, src := range cluster.Spec.TLS.AdditionalClientCAs {
			pemData, rerr := readTrustedCA(ctx, r, cluster.Namespace, src)
			if rerr != nil {
				return nil, 0, false, rerr
			}
			certs, perr := parsePEMCerts(pemData)
			if perr != nil {
				return nil, 0, false, fmt.Errorf("additionalClientCAs: %w", perr)
			}
			if len(certs) == 0 {
				return nil, 0, false, fmt.Errorf("additionalClientCAs: %s holds no PEM CERTIFICATE", describeCASource(src))
			}
			inputs = append(inputs, pemData)
		}
	}
	bundle, count, err = mergeCABundle(inputs...)
	return bundle, count, clusterCAMissing, err
}

func describeCASource(src riakv1.TrustedCASource) string {
	if src.SecretRef != nil {
		return fmt.Sprintf("Secret %q key %q", src.SecretRef.Name, src.SecretRef.Key)
	}
	if src.ConfigMapRef != nil {
		return fmt.Sprintf("ConfigMap %q key %q", src.ConfigMapRef.Name, src.ConfigMapRef.Key)
	}
	return "entry"
}

// reconcileTrustBundle writes the operator-owned trust Secret when the cluster
// trusts additional client CAs. It writes nothing until the cluster certificate
// has been issued (the pods cannot start before then anyway), and does not
// rewrite an unchanged Secret.
func (r *RiakClusterReconciler) reconcileTrustBundle(ctx context.Context, cluster *riakv1.RiakCluster) error {
	if !hasAdditionalClientCAs(cluster) {
		return nil
	}
	rd := reader(r.APIReader, r.Client)
	bundle, _, clusterCAMissing, err := buildTrustBundle(ctx, rd, cluster)
	if err != nil {
		return err
	}
	if clusterCAMissing {
		log.FromContext(ctx).Info("cluster certificate not issued yet; trust bundle will follow", "cluster", cluster.Name)
		return nil
	}

	key := client.ObjectKey{Namespace: cluster.Namespace, Name: clusterTrustSecretName(cluster.Name)}
	existing := &corev1.Secret{}
	switch gerr := rd.Get(ctx, key, existing); {
	case apierrors.IsNotFound(gerr):
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Data:       map[string][]byte{trustBundleKey: bundle},
		}
		if err := controllerutil.SetControllerReference(cluster, s, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, s)
	case gerr != nil:
		return gerr
	}
	if bytes.Equal(existing.Data[trustBundleKey], bundle) {
		return nil
	}
	existing.Data = map[string][]byte{trustBundleKey: bundle}
	return r.Update(ctx, existing)
}

// trustStatus reports the trust bundle's size and any problem building it, for
// status.tlsStatus.
func (r *RiakClusterReconciler) trustStatus(ctx context.Context, cluster *riakv1.RiakCluster) (int32, string) {
	if cluster.Spec.TLS == nil || !cluster.Spec.TLS.Enabled {
		return 0, ""
	}
	_, count, _, err := buildTrustBundle(ctx, reader(r.APIReader, r.Client), cluster)
	if err != nil {
		return 0, err.Error()
	}
	return int32(count), ""
}

// verifyClientCert checks an externally issued client certificate (PEM, leaf
// first, then any intermediates): the CommonName must be username, the
// certificate must be valid at now for client authentication, and chain to one
// of roots. It returns the leaf's expiry.
func verifyClientCert(certPEM []byte, username string, roots *x509.CertPool, now time.Time) (time.Time, error) {
	certs, err := parsePEMCerts(certPEM)
	if err != nil {
		return time.Time{}, err
	}
	if len(certs) == 0 {
		return time.Time{}, fmt.Errorf("tls.crt holds no PEM CERTIFICATE")
	}
	leaf := certs[0]
	if leaf.Subject.CommonName != username {
		return time.Time{}, fmt.Errorf("certificate CommonName %q does not match spec.username %q",
			leaf.Subject.CommonName, username)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return time.Time{}, fmt.Errorf("certificate is not valid for client authentication against a trusted CA: %w", err)
	}
	return leaf.NotAfter, nil
}

// externalCertificateReadiness validates the Secret named by
// spec.certificateRef.externalSecretName against the cluster's trusted CAs. It
// returns readiness, a human reason when not ready, and the certificate expiry.
func externalCertificateReadiness(
	ctx context.Context, r client.Reader, user *riakv1.RiakUser, cluster *riakv1.RiakCluster, now time.Time,
) (bool, string, time.Time) {
	name := user.Spec.CertificateRef.ExternalSecretName
	s := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: user.Namespace, Name: name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("Secret %q does not exist", name), time.Time{}
		}
		return false, err.Error(), time.Time{}
	}
	crt, ok := s.Data["tls.crt"]
	if !ok {
		return false, fmt.Sprintf("Secret %q has no tls.crt", name), time.Time{}
	}
	bundle, _, _, err := buildTrustBundle(ctx, r, cluster)
	if err != nil {
		return false, "cannot build the cluster's trusted CAs: " + err.Error(), time.Time{}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		return false, "the cluster trusts no CA: enable spec.tls and/or set spec.tls.additionalClientCAs", time.Time{}
	}
	notAfter, err := verifyClientCert(crt, user.Spec.Username, roots, now)
	if err != nil {
		return false, err.Error(), time.Time{}
	}
	return true, "", notAfter
}
