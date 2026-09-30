---
name: operand-local-arm64-build
description: How to build and run the Riak operand image locally on the user's Apple Silicon host for OpenShift-style validation
metadata:
  type: reference
---

The user's host is Apple Silicon (arm64), with Rancher Desktop docker at `~/.rd/bin/docker` (not always the default `docker` on PATH — prepend `PATH="$HOME/.rd/bin:$PATH"` in Bash calls to be safe). `docker` server responds (v29.x).

Build the operand for the host arch: `make docker-build-riak` → `ghcr.io/marthydavid/riak:3.2.6`. The arm64 build uses the Amazon Linux base + Graviton RPM and builds natively/fast (~20-40s after base pull). Needs network to pull the base image and the Riak RPM from files.tiot.jp.

**Simulate OpenShift restricted-v2 locally** (arbitrary UID, sole group 0):
```
docker run --user 1000700000:0 ghcr.io/marthydavid/riak:3.2.6
```
Must reach `Riak is ready (ping=pong).`. Since the entrypoint blocks on `wait`, run detached (`-d`) and poll `docker logs` for the ready line, then `docker rm -f`. macOS has no `timeout` (would need `gtimeout` via coreutils) — don't rely on it; use a detached container + poll loop instead.

See [[operand-openshift-scc]] for the fix this validates.
