# Claude Code Guidance for OpenRiak Operator

## Build and Test

```bash
go build ./...                         # verify it compiles
go test ./internal/... -timeout 180s   # run all unit tests (envtest + riak package)
go test ./internal/... -coverprofile cover.out -timeout 180s && go tool cover -func cover.out
```

Coverage target: **≥85%** on `internal/controller` and `internal/riak`.

The `test/e2e` package requires a live Kubernetes cluster; expected to fail locally.

### Linting locally

CI runs golangci-lint v2.14.0 (`.github/workflows/lint.yml`; the config is golangci-lint v2 format).
Run the same pinned version, or it reports nothing useful:

```bash
make lint                            # installs the pinned v2.14.0 into bin/ and runs it
hack/pre-push.sh                     # gofmt + vet + pinned lint + unit tests: run before EVERY push
```

A globally installed `golangci-lint` may be a different major version that cannot read
`.golangci.yml`; do not use it. See `.claude/skills/pre-push/SKILL.md`.

The module needs Go 1.25+ (`go.mod`). Targeted platforms: OpenShift/OKD 4.21 (Kubernetes 1.34) and
4.22 (1.35) and AKS (1.34+); controller-runtime v0.23 / k8s libraries 0.35 / envtest 1.35.

Watch `gocyclo` in particular: `RiakUserReconciler.Reconcile` sits near the limit of 30, so a
couple of added branches trip it. Extract into a helper rather than raising the threshold.

## Testability Patterns

### Executor injection

`internal/riak.Executor` holds a `runnerFn` field (the real kubectl shell runner).  
Both `RiakBucketReconciler` and `RiakUserReconciler` have an `Executor *riak.Executor` field:

- `nil` → a real executor is created on each reconcile (production behaviour)
- non-nil → injected executor is used (test behaviour)

Use `riak.NewExecutorWithRunner(logr.Discard(), noopRunner)` to inject a no-op runner in tests:

```go
noopRunner := func(_ context.Context, _ string, _ ...string) (string, error) { return "", nil }
r := &RiakBucketReconciler{
    Client:   k8sClient,
    Scheme:   k8sClient.Scheme(),
    Executor: riak.NewExecutorWithRunner(logr.Discard(), noopRunner),
}
```

### Controller unit tests use envtest

`internal/controller/suite_test.go` starts a real etcd + kube-apiserver via `envtest.Environment`.  
Use `k8sClient` (type `client.Client`) and `cfg` (`*rest.Config`) provided by the suite.

Pattern for each controller:
1. Create the resource via `k8sClient.Create`
2. Call `Reconcile` directly (not via manager watch loop)
3. Inspect status with `k8sClient.Get`

### Pod creation in envtest requires Spec.Containers

envtest validates Pod specs. Always include at least one container:

```go
pod := &corev1.Pod{
    ...
    Spec: corev1.PodSpec{
        Containers: []corev1.Container{{Name: "riak", Image: "basho/riak-kv:latest"}},
    },
}
```

Update pod status separately via `k8sClient.Status().Update(ctx, pod)`.

### Cluster status must be forced in tests

`Status().Update` is required to set `cluster.Status.Phase = riakv1.PhaseReady` because the status
subresource is separate from the main resource:

```go
c.Status.Phase = riakv1.PhaseReady
c.Status.Members = []riakv1.RiakNodeMember{{Pod: clusterName + "-0", Name: clusterName + "-0"}}
Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
```

## CRD Validation Constraints

These enum constraints are enforced by the API server — do not write controller code that handles
values outside these ranges, as it becomes dead code:

| Field | Valid values |
|-------|-------------|
| `RiakUser.spec.grants[].resource` | `"bucket"`, `"any"` |
| `RiakUser.spec.grants[].permission` | `"read"`, `"write"`, `"delete"`, `"list"`, `"admin"` |

## Riak CLI Format

`riak-admin bucket-type create` requires JSON, not key=value:

```bash
riak-admin bucket-type create mytype '{"props":{"n_val":3}}'
```

The executor's `CreateBucket` method handles this serialisation. String values that parse as a
JSON literal (number, bool) are sent as their native type. An already-active type (or Riak's
built-in `default`) cannot be created, so `CreateBucket` falls back to `bucket-type update`.

**riak-admin exits 0 when a command fails.** Riak's reply is printed instead: a trailing
`error` line (bucket-type commands) or an `{error,Reason}` term (security commands).
`ExecuteRiakAdmin` turns such replies into an error carrying the reason (`riakAdminFailed`);
idempotent callers match the specific reason they tolerate (`role_exists` for add-user,
`unknown_user` for del-user, `already_active`/`default_type` for bucket-type create). Test mocks
should return real riak-admin output rather than a Go error to exercise this path.

### Reconciled Riak state

- `spec.grants` is reconciled: `GrantUserPermissions` grants the spec, reads `print-grants`, and
  revokes anything not in the spec (empty spec revokes all).
- Deleting a RiakUser runs `security del-user` (best effort; skipped when the cluster is
  missing/deleting/not Ready; gives up after `userDeleteGracePeriod`).
- `test/scale` verifies Riak against the CRs on every node (`-verify`, `-verify-only`, `-mutate`,
  `-delete-users-every`); see `docs/scaling.md`.
- `test/scale -soak` (`make soak-test`) holds a constant load for hours, watches for OOM kills, restarts,
  memory and disk, and scales memory/nodes by a unit-tested policy (`soak_policy.go`). Things learned the
  hard way: size the load for the **network** first (`1.04 x ops/s x object size` per node must stay under
  the link, ~90 MB/s on 1 GbE; check `/sys/class/net/*/speed`); load-client and Riak CPU are *requests*, not
  limits, and a nearly full request budget leaves clients `Pending`; run it detached
  (`nohup setsid ... > log &`) because a tool or terminal timeout kills a 4 h run; use `-soak-artifacts`
  to keep the time series and logs, and `hack/soak-report.py` (`make test-soak-report`) to chart them; a
  write that times out may still land, so the client counts it as `landed`, not `corrupt`.

## Container Images

All images are published to **GitHub Container Registry** under `ghcr.io/marthydavid/`:

| Image | Registry path | Base |
|-------|--------------|------|
| Operator | `ghcr.io/marthydavid/openriak-operator:<tag>` | Go 1.25 / alpine |
| Riak KV 3.0 | `ghcr.io/marthydavid/riak:3.0.16` (alias `3.0`) | amd64: UBI8/el8 OTP22.3; arm64: AL2/graviton3 OTP22 |
| Riak KV 3.2 (default, `latest`) | `ghcr.io/marthydavid/riak:3.2.6` (alias `3.2`) | amd64: UBI8/el8 OTP24; arm64: AL2023/graviton2 OTP24 |
| Riak KV 3.4 | `ghcr.io/marthydavid/riak:3.4.0` (alias `3.4`) | amd64 only: UBI9/el9 OTP26 (all upstream aarch64 RPMs are graviton3/SVE → SIGILL on generic arm64; no non-RPM bases used) |

The Riak image is built from `images/riak/Dockerfile`, **multi-arch**: amd64 uses a Red Hat UBI
base with the RHEL x86_64 RPM, arm64 uses an Amazon Linux base with the Graviton aarch64 RPM
(installed `--nodeps` with a bundled-ERTS escript symlink). Only RPM-based distributions are
used; versions whose sole aarch64 RPMs are graviton3/SVE builds (3.4.0 — they SIGILL on
non-SVE arm64 like Apple Silicon, Ampere, Graviton2) are published amd64-only. Package URLs
are irregular across versions, so each published version carries full URLs in the build-riak
workflow matrix. 3.0.18 is ubuntu-only upstream, hence 3.0.16. amzn2 (3.0.x arm64) needs
openssl11-libs for the libcrypto.so.1.1 the Graviton build links.
The operator image is built from the root `Dockerfile` (multi-arch amd64+arm64).

Build locally:

```bash
make docker-build-riak                        # ghcr.io/marthydavid/riak:3.2.6
make docker-push-riak
make docker-build IMG=ghcr.io/marthydavid/openriak-operator:dev
make docker-push  IMG=ghcr.io/marthydavid/openriak-operator:dev
```

### CI workflows

| Workflow | File | Triggers | Pushes to |
|----------|------|----------|-----------|
| Build Operator Image | `.github/workflows/build-operator.yml` | push `main`, tags `v*`, PRs | `ghcr.io/marthydavid/openriak-operator` |
| Build Riak Image | `.github/workflows/build-riak.yml` | push `main`/tags `riak-*` when `images/riak/**` changes, PRs | `ghcr.io/marthydavid/riak` |

Operator tags: semver on `v*` tags, short-SHA on every push, `latest` on `main`.  
Riak tags: patch + minor alias per matrix entry (`3.0.16`/`3.0`, `3.2.6`/`3.2`, `3.4.0`/`3.4`); `latest` follows the 3.2.x default on `main`.  
Same-repo PRs also push **non-production** tags only: `openriak-operator:pr-<N>` (+ short SHA) and `riak:<version>-pr-<N>`. They never write the production tags (`3.2.6`, `3.2`, `latest`). Fork PRs build without pushing (no write token).

The controller's fallback image (when `spec.image` is omitted) is `ghcr.io/marthydavid/riak:3.2.6`.

## Security Notes

### Known False Positive Patterns (do not re-flag these)

| Pattern | Why it's safe |
|---------|--------------|
| Command injection via kubectl exec | `executor.go` uses `exec.CommandContext` with argument array — no shell is invoked; metacharacters are literal |
| `RIAK_CONFIG_*` env var injection | Requires Kubernetes RBAC write access to RiakCluster; env vars are a trusted boundary |
| Config key-value logging in `SetConfig` | Logged Riak node config params contain no credential material; authentication uses mTLS client certificates, which never pass through `SetConfig` |

### Resolved: hardcoded default password

Password authentication was removed entirely: `RiakUser.spec.certificateRef` is required and
users authenticate by mTLS client certificate (CN == username). There is no password field,
no password executor path, and therefore no default password.

## Finalizer Pattern

Every controller follows this order in Reconcile:

1. Get the resource; ignore NotFound
2. **Handle deletion first** (`if !DeletionTimestamp.IsZero()`)
3. Add finalizer if absent
4. Initialise status if empty
5. Do business logic

## Status Reporting

`internal/controller/status.go` holds the shared status helpers. Rules the controllers follow:

- **Status describes what was applied, not what was requested.** `RiakBucket.status.properties`
  is the exact property set sent to `riak-admin`, i.e. `spec.properties` with the typed fields
  (`nVal`/`replicationFactor` → `n_val`, `allowMulti` → `allow_mult`) layered on top by
  `effectiveBucketProperties`. Typed fields win over the same key in `spec.properties`;
  `allow_mult` is only written when `spec.allowMulti` is true, because a plain `false` cannot be
  told apart from "unset".
- **Conditions go through `setCondition`**, which wraps `meta.SetStatusCondition` so
  LastTransitionTime survives unchanged states. It returns whether anything changed — use that to
  skip status writes on paths that requeue every few seconds (`failBucket`, `failUser`, the
  "cluster not ready" waits), otherwise every pending resource writes status continuously.
- **Certificate readiness is not part of the phase.** `RiakUser.status.phase: Ready` means the
  Riak-side identity exists; cert-manager issuance is asynchronous and is reported in
  `certificateReady` / `certificateError` plus the `CertificateReady` condition. The user reconcile
  requeues (30s) until the certificate is observed issued.
- **Cluster status is recomputed from live objects on every reconcile** (10s requeue): pods for
  node health, PVCs (`data-<pod>`) for `storageReady`, the cert-manager Certificate for
  `tlsStatus`, pod container statuses plus the ServiceMonitor for `monitoringStatus`, and the
  namespace's RiakBuckets/RiakUsers for `buckets`/`users`. Lists are sorted by name so the status
  does not churn on map/list ordering.

## Ring size

Use `ring_size` >= 128 (a power of two). A ring is split into whole partitions, so a small ring
cannot be balanced across a few nodes (8 partitions over 3 nodes is 4/2/2 = 50/25/25 %). The Riak
entrypoint defaults to 128, and `test/scale` refuses `-ring-size` below 128 and fails verification
when any node owns more or fewer partitions than `ring_size/nodes` rounded down/up. `ring_size`
cannot be changed on an existing cluster; recreate it.

## Releasing

Releases (operator `v*` tag + chart `chart-v*` tag, version bumps across all references, release
notes, CI verification) are described in the `release` skill: `.claude/skills/release/SKILL.md`.
Ask Claude to "cut a release" and it follows that skill. Always release from `main`.
