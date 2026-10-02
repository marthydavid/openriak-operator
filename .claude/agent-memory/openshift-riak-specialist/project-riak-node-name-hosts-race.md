---
name: project-riak-node-name-hosts-race
description: Issue #59 root cause - kubelet rewrites pod /etc/hosts (truncate+write) per container start; Riak entrypoint read it empty and named the node riak@<pod>, poisoning the PVC ring
metadata:
  type: project
---

Issue #59 (seed crash-loop with `riak_core_capability` `orddict:fetch(<fqdn>, [])` + "Hostname <pod> is illegal")
was traced on metal1 (2026-10-02, branch fix/riak-node-crash-59):

- The kubelet regenerates the pod's shared etc-hosts file with a truncating write each time it creates a
  container of the pod. With `spec.monitoring` the metrics-exporter sidecar is created ~25 ms after the riak
  container starts, so the entrypoint's single `/etc/hosts` read can see an EMPTY file. Probe: a container
  re-reading /etc/hosts while 2 sidecars start saw an empty read in 52 of 72 pod starts.
- Empty read -> old entrypoint fell back to `riak@$POD_NAME` (dotless long name). Node runs, TCP 8087 Ready,
  but riak-admin answers `Node riak@<pod> is not responding to pings` with EXIT 0 (operator logged "seed node
  not found in member-status"), entrypoint readiness gate times out (120 s) -> container exits.
- Ring persisted under `riak@<pod>`; every restart (FQDN name) crashes in capability negotiation. Reproduced
  exactly by starting a pod without POD_IP then restarting it with POD_IP on the same PVC.
- Decode a ring: `{ok,B}=file:read_file(F), R=binary_to_term(B), element(8,R)` = members (#chstate_v2).
  escript in the image needs the bundled erts bin dir on PATH (`erl` is not on PATH).
- Bare `kubectl exec ... riak-admin` never works on these pods (addresses riak@<short>); always set
  VMARGS_PATH to the newest /var/lib/riak/generated.conf/vm.*.args.

**Why:** explains a class of "first start only, intermittent, monitoring on" failures.
**How to apply:** any entrypoint logic reading /etc/hosts must retry/fallback; treat "not responding to pings"
as an error; see [[project-operand-openshift-scc]] for other operand quirks.
