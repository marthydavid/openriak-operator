---
name: operand-openshift-scc
description: How the Riak operand image was made restricted-v2 compatible (arbitrary-UID / group-0 writable dirs) and where that fix lives
metadata:
  type: project
---

The Riak operand image (`images/riak/Dockerfile`) was made OpenShift `restricted-v2` compatible in PR #28 (branch `fix/operand-openshift-arbitrary-uid`, merged to main by the user).

**Root cause:** restricted-v2 ignores the image `USER` and runs the container as an arbitrary non-root UID (e.g. `1000700000`) whose only supplementary group is `0` (root). The operand's runtime-writable dirs were owned `riak:riak` mode `0755`, so an arbitrary UID could not write. The entrypoint (`images/riak/scripts/entrypoint.sh`, `set -eo pipefail`) writes at runtime — `cat > /etc/riak/riak.conf`, `touch /var/log/riak/console.log`, PID under `/var/run/riak`, data under `/var/lib/riak` — and crashed on the first write → CrashLoopBackOff. An `fsGroup` mount only rescues the `/var/lib/riak` PVC; the ephemeral `/etc/riak`, `/var/log/riak`, `/var/run/riak` are baked into the image.

**The fix** (permissions only, no base/package changes; RPM-distro constraint respected) is in the shared `# ── common ──` stage after `FROM base-${TARGETARCH}` — a single edit covers both amd64 UBI and arm64 Amazon Linux bases:
```
chown -R riak:0 /etc/riak /var/lib/riak /var/log/riak /var/run/riak
chmod -R g=u    /etc/riak /var/lib/riak /var/log/riak /var/run/riak
```
`g=u` grants the root group owner-equivalent perms; assigned UIDs are always in group 0. No-op on vanilla k8s (still runs as `USER riak`, still owner). Dirs end up `drwxrwxr-x riak root`.

**Why:** production target is OpenShift on restricted-v2; the operand must not require a custom SCC.

**How to apply:** When touching the operand Dockerfile or entrypoint, preserve group-0 + group-writable perms on any new runtime-writable path. Do NOT add runtime `mkdir`/`chown` in the entrypoint (would fail as arbitrary UID). Verify locally by simulating OpenShift: `docker run --user 1000700000:0 ghcr.io/marthydavid/riak:3.2.6` must reach "Riak is ready (ping=pong)." — this is the canonical proof, not just a plain run. Related: operator StatefulSet securityContext work is separate (not done in PR #28). See [[operand-local-arm64-build]].
