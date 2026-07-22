# Riak KV container image, multi-arch.
# RPM source: https://files.tiot.jp/riak/kv/
#
# Upstream ships x86_64 RPMs for RHEL and aarch64 RPMs only as Amazon Linux
# Graviton builds, so the two architectures use different bases:
#   amd64 -> Red Hat UBI + el RPM (EPEL provides the erlang-erts dependency)
#   arm64 -> Amazon Linux + Graviton RPM (installed --nodeps: the escript
#            dependency is unsatisfiable there; Riak bundles its own ERTS)
#
# RPM URLs are passed whole because upstream's layout is irregular across
# versions (el8 vs el9, amzn2 vs amzn2023, three spellings of the graviton
# directory). The published version/URL combinations live in the build-riak
# workflow matrix. Defaults build the 3.2.6 flagship.
#
# Versions whose only Graviton RPMs target graviton3 (3.4.0: both OTP24 and
# OTP26 builds verified to SIGILL — SVE instructions — on non-SVE arm64 like
# Apple Silicon, Ampere and Graviton2) are published amd64-only; only
# RPM-based distributions are used.

ARG RHEL_VERSION=8
ARG AMZN_VERSION=2023

# ── amd64: Red Hat UBI ────────────────────────────────────────────────────────
FROM registry.access.redhat.com/ubi${RHEL_VERSION}/ubi AS base-amd64

ARG RHEL_VERSION
ARG AMD64_RPM_URL=https://files.tiot.jp/riak/kv/3.2/3.2.6/rhel/8/riak-3.2.6.OTP24-1.el8.x86_64.rpm
ARG AMD64_RPM_SHA256=63363abccafe83db9d440a9968dc436a0cd2753d0665093aff6d1a556f3e2c05

# EPEL satisfies the /usr/bin/escript dependency declared by the Riak RPM spec
# (provided by erlang-erts). Riak bundles its own OTP runtime; erlang-erts is
# only needed for the RPM dependency check. procps-ng: `ps` for riak scripts.
RUN dnf install -y \
        "https://dl.fedoraproject.org/pub/epel/epel-release-latest-${RHEL_VERSION}.noarch.rpm" \
    && dnf install -y \
        procps-ng \
        erlang-erts \
    && curl -fsSLo /tmp/riak.rpm "${AMD64_RPM_URL}" \
    && echo "${AMD64_RPM_SHA256}  /tmp/riak.rpm" | sha256sum -c - \
    && dnf install -y /tmp/riak.rpm \
    && rm -f /tmp/riak.rpm \
    && dnf clean all \
    && rm -rf /var/cache/dnf

# ── arm64: Amazon Linux + Graviton RPM ───────────────────────────────────────
FROM amazonlinux:${AMZN_VERSION} AS base-arm64

ARG ARM64_PKG_URL=https://files.tiot.jp/riak/kv/3.2/3.2.6/amazon/2023%20(graviton%202)/riak-3.2.6.OTP24-1.amzn2023.aarch64.rpm
ARG ARM64_PKG_SHA256=a62366fb0e2e90f55a36c05993d028db24bd831696b1a031dc61cfd0c2d200a9

# yum works on both amzn2 (real yum) and amzn2023 (dnf alias).
# shadow-utils: useradd; the riak user is pre-created so the RPM %pre is a
# no-op regardless of version. --nodeps: no erlang packages exist here to
# satisfy the escript dependency; the bundled ERTS provides it (symlinked).
# amzn2 ships OpenSSL 1.0 but the Graviton Riak builds link libcrypto.so.1.1,
# provided there by the openssl11-libs package (already OpenSSL 3 on amzn2023).
RUN yum install -y procps-ng shadow-utils hostname findutils tar gzip \
    && if [ "$(rpm -E '%{amzn}')" = "2" ]; then yum install -y openssl11-libs; fi \
    && yum clean all \
    && groupadd -r riak \
    && useradd -r -g riak -d /var/lib/riak -s /sbin/nologin riak \
    && curl -fsSLo /tmp/riak.rpm "${ARM64_PKG_URL}" \
    && echo "${ARM64_PKG_SHA256}  /tmp/riak.rpm" | sha256sum -c - \
    && rpm -ivh --nodeps /tmp/riak.rpm \
    && rm -f /tmp/riak.rpm \
    && ln -sf /usr/lib64/riak/erts-*/bin/escript /usr/bin/escript

# ── common ────────────────────────────────────────────────────────────────────
ARG TARGETARCH
FROM base-${TARGETARCH}

# Ensure directories are writable by the riak user and by an arbitrary
# OpenShift-assigned UID.
# /var/run/riak is the hardcoded PID_DIR used by `riak start` (daemon mode) and is
# not creatable by the non-root riak user at runtime, so pre-create it here.
#
# OpenShift's restricted-v2 SCC ignores the image USER and runs the container as
# an arbitrary non-root UID whose only supplementary group is 0 (root). Assign
# these runtime-writable dirs to group 0 and make them group-writable (g=u grants
# the root group the owner's permissions) so any assigned UID can write to the
# ephemeral /etc/riak, /var/log/riak and /var/run/riak (an fsGroup mount only
# covers the /var/lib/riak PVC). This is a no-op on vanilla Kubernetes, where the
# container still runs as USER riak and writes as the owner.
RUN mkdir -p /var/run/riak \
    && chown -R riak:0 /etc/riak /var/lib/riak /var/log/riak /var/run/riak \
    && chmod -R g=u    /etc/riak /var/lib/riak /var/log/riak /var/run/riak

COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 755 /usr/local/bin/entrypoint.sh

# Data directory — mount a PersistentVolume here.
VOLUME ["/var/lib/riak"]

EXPOSE 8087 8098

USER riak

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
