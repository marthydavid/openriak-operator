#!/usr/bin/env bash
# Run what CI runs, before pushing: gofmt, go vet, golangci-lint (the pinned v2.14.0,
# exactly like .github/workflows/lint.yml), and the unit tests. Exits non-zero on the first failure.
#
#   hack/pre-push.sh            # everything
#   FAST=1 hack/pre-push.sh     # skip the slow envtest controller tests
#
# The pinned golangci-lint is installed once into $LOCALBIN (default: a shared cache outside the
# checkout, so git worktrees reuse it instead of rebuilding it).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

export LOCALBIN="${LOCALBIN:-$HOME/.cache/openriak-operator/bin}"

echo "== gofmt"
unformatted="$(gofmt -l $(git ls-files '*.go'))"
if [ -n "$unformatted" ]; then
  echo "not gofmt-formatted:"; echo "$unformatted"; exit 1
fi

echo "== go vet"
go vet ./...

# Use the Makefile's pinned golangci-lint rather than a globally installed one, which may be a
# different major version and cannot be trusted to read this repo's config.
echo "== golangci-lint v2.14.0 (as CI)"
make lint

echo "== unit tests"
pkgs="./internal/riak ./test/scale ./test/manifests"
if [ "${FAST:-}" != "1" ]; then
  pkgs="./internal/... ./test/scale ./test/manifests"
  # The controller tests run against a real etcd + kube-apiserver (envtest). Provision it the way
  # `make test` does, otherwise their BeforeSuite fails and every controller test is skipped.
  make envtest >/dev/null
  k8s_version="$(sed -n 's/^ENVTEST_K8S_VERSION *= *//p' Makefile)"
  KUBEBUILDER_ASSETS="$("$LOCALBIN/setup-envtest" use "$k8s_version" --bin-dir "$LOCALBIN" -p path)"
  export KUBEBUILDER_ASSETS
fi
# shellcheck disable=SC2086  # intentional word splitting of the package list
go test $pkgs -timeout 180s

echo "pre-push checks passed"
