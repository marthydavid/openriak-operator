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

package v1

import "testing"

// ---------- RiakUserSpec.DeepCopy / DeepCopyInto ----------

func TestRiakUserSpecDeepCopy_CertificateRefIsIndependentCopy(t *testing.T) {
	spec := &RiakUserSpec{
		ClusterName: "my-cluster",
		Username:    "alice",
		CertificateRef: &UserCertificateRef{
			IssuerRef:  CertIssuerRef{Name: "ca-issuer", Kind: "Issuer"},
			SecretName: "alice-client-tls",
		},
	}

	out := spec.DeepCopy()

	if out == spec {
		t.Fatal("DeepCopy returned the same pointer as the original")
	}
	if out.CertificateRef == spec.CertificateRef {
		t.Fatal("expected CertificateRef to be a distinct pointer after DeepCopy")
	}
	if *out.CertificateRef != *spec.CertificateRef {
		t.Fatalf("expected copied CertificateRef to equal original, got %+v vs %+v", out.CertificateRef, spec.CertificateRef)
	}

	// Mutating the copy must not affect the original.
	out.CertificateRef.IssuerRef.Name = "mutated-issuer"
	if spec.CertificateRef.IssuerRef.Name != "ca-issuer" {
		t.Fatalf("mutating the copy's CertificateRef leaked into the original: %q", spec.CertificateRef.IssuerRef.Name)
	}
}

func TestRiakUserSpecDeepCopy_NilCertificateRef(t *testing.T) {
	spec := &RiakUserSpec{ClusterName: "my-cluster", Username: "bob"}

	out := spec.DeepCopy()

	if out.CertificateRef != nil {
		t.Fatalf("expected nil CertificateRef to remain nil after DeepCopy, got %+v", out.CertificateRef)
	}
}

func TestRiakUserSpecDeepCopy_GrantsAreIndependentSlice(t *testing.T) {
	spec := &RiakUserSpec{
		ClusterName: "my-cluster",
		Username:    "carol",
		CertificateRef: &UserCertificateRef{
			IssuerRef: CertIssuerRef{Name: "ca-issuer"},
		},
		Grants: []Grant{{Resource: "any", Permission: "read"}},
	}

	out := spec.DeepCopy()
	out.Grants[0].Permission = "write"

	if spec.Grants[0].Permission != "read" {
		t.Fatalf("mutating the copy's Grants leaked into the original: %q", spec.Grants[0].Permission)
	}
}

func TestRiakUserSpecDeepCopy_NilReceiver(t *testing.T) {
	var spec *RiakUserSpec
	if got := spec.DeepCopy(); got != nil {
		t.Fatalf("expected nil DeepCopy for nil receiver, got %+v", got)
	}
}

// ---------- UserCertificateRef.DeepCopy ----------

func TestUserCertificateRefDeepCopy(t *testing.T) {
	ref := &UserCertificateRef{
		IssuerRef:  CertIssuerRef{Name: "ca-issuer", Kind: "ClusterIssuer"},
		SecretName: "custom-secret",
	}

	out := ref.DeepCopy()

	if out == ref {
		t.Fatal("DeepCopy returned the same pointer as the original")
	}
	if *out != *ref {
		t.Fatalf("expected copy to equal original, got %+v vs %+v", out, ref)
	}

	out.IssuerRef.Kind = "Issuer"
	if ref.IssuerRef.Kind != "ClusterIssuer" {
		t.Fatalf("mutating the copy leaked into the original: %q", ref.IssuerRef.Kind)
	}
}

func TestUserCertificateRefDeepCopy_NilReceiver(t *testing.T) {
	var ref *UserCertificateRef
	if got := ref.DeepCopy(); got != nil {
		t.Fatalf("expected nil DeepCopy for nil receiver, got %+v", got)
	}
}