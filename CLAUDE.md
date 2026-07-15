# Claude Code Guidance for OpenRiak Operator

## Build and Test

```bash
go build ./...                         # verify it compiles
go test ./internal/... -timeout 180s   # run all unit tests (envtest + riak package)
go test ./internal/... -coverprofile cover.out -timeout 180s && go tool cover -func cover.out
```

Coverage target: **≥85%** on `internal/controller` and `internal/riak`.

The `test/e2e` package requires a live Kubernetes cluster; expected to fail locally.

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
JSON literal (number, bool) are sent as their native type.

## Container Images

All images are published to **GitHub Container Registry** under `ghcr.io/marthydavid/`:

| Image | Registry path |
|-------|--------------|
| Operator | `ghcr.io/marthydavid/openriak-operator:<tag>` |
| Riak KV 3.2 | `ghcr.io/marthydavid/riak:3.2.6` |

The Riak image is built from `images/riak/Dockerfile` (UBI9 base, RPM from files.tiot.jp).  
The operator image is built from the root `Dockerfile` (Go 1.22 / alpine, multi-arch amd64+arm64).

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
Riak tags: version extracted from `ARG RIAK_VERSION` in the Dockerfile, `latest` on `main`.  
PRs build but do **not** push (no registry credentials needed).

The controller's fallback image (when `spec.image` is omitted) is `ghcr.io/marthydavid/riak:3.2.6`.

## Security Notes

### Known False Positive Patterns (do not re-flag these)

| Pattern | Why it's safe |
|---------|--------------|
| Command injection via kubectl exec | `executor.go` uses `exec.CommandContext` with argument array — no shell is invoked; metacharacters are literal |
| `RIAK_CONFIG_*` env var injection | Requires Kubernetes RBAC write access to RiakCluster; env vars are a trusted boundary |
| Config key-value logging in `SetConfig` | Only logs Riak node config params; no credentials exist — users authenticate by client certificate only |

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
