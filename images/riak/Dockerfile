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

ARG RHEL_VERSION=8
ARG AMZN_VERSION=2023
ARG UBUNTU_VERSION=24.04
# arm64 package flavor: "rpm" (Amazon Linux Graviton build) or "deb" (Ubuntu
# arm64 build). Graviton3-targeted RPMs use SVE instructions and SIGILL on
# non-SVE arm64 (Apple Silicon, Ampere, Graviton2) — versions that only ship
# graviton3 RPMs use the generic Ubuntu arm64 deb instead.
ARG ARM64_VARIANT=rpm

# ── amd64: Red Hat UBI ────────────────────────────────────────────────────────
FROM registry.access.redhat.com/ubi${RHEL_VERSION}/ubi AS base-amd64

ARG RHEL_VERSION
ARG AMD64_RPM_URL=https://files.tiot.jp/riak/kv/3.2/3.2.6/rhel/8/riak-3.2.6.OTP24-1.el8.x86_64.rpm

# EPEL satisfies the /usr/bin/escript dependency declared by the Riak RPM spec
# (provided by erlang-erts). Riak bundles its own OTP runtime; erlang-erts is
# only needed for the RPM dependency check. procps-ng: `ps` for riak scripts.
RUN dnf install -y \
        "https://dl.fedoraproject.org/pub/epel/epel-release-latest-${RHEL_VERSION}.noarch.rpm" \
    && dnf install -y \
        procps-ng \
        erlang-erts \
    && curl -fsSLo /tmp/riak.rpm "${AMD64_RPM_URL}" \
    && dnf install -y /tmp/riak.rpm \
    && rm -f /tmp/riak.rpm \
    && dnf clean all \
    && rm -rf /var/cache/dnf

# ── arm64 (rpm): Amazon Linux + Graviton RPM ─────────────────────────────────
FROM amazonlinux:${AMZN_VERSION} AS base-arm64-rpm

ARG ARM64_PKG_URL=https://files.tiot.jp/riak/kv/3.2/3.2.6/amazon/2023%20(graviton%202)/riak-3.2.6.OTP24-1.amzn2023.aarch64.rpm

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
    && rpm -ivh --nodeps /tmp/riak.rpm \
    && rm -f /tmp/riak.rpm \
    && ln -sf /usr/lib64/riak/erts-*/bin/escript /usr/bin/escript

# ── arm64 (deb): Ubuntu + generic arm64 deb ──────────────────────────────────
FROM ubuntu:${UBUNTU_VERSION} AS base-arm64-deb

ARG ARM64_PKG_URL=https://files.tiot.jp/riak/kv/3.4/3.4.0/ubuntu/noble64/riak_3.4.0-OTP26_arm64.deb

RUN apt-get update \
    && apt-get install -y --no-install-recommends curl ca-certificates procps \
    && curl -fsSLo /tmp/riak.deb "${ARM64_PKG_URL}" \
    && (dpkg -i /tmp/riak.deb || apt-get install -y -f --no-install-recommends) \
    && rm -f /tmp/riak.deb \
    && rm -rf /var/lib/apt/lists/* \
    && id riak >/dev/null 2>&1 || useradd -r -d /var/lib/riak -s /usr/sbin/nologin riak

# ── arm64: selected flavor ────────────────────────────────────────────────────
ARG ARM64_VARIANT
FROM base-arm64-${ARM64_VARIANT} AS base-arm64

# ── common ────────────────────────────────────────────────────────────────────
ARG TARGETARCH
FROM base-${TARGETARCH}

# Ensure directories are writable by the riak user.
# /var/run/riak is the hardcoded PID_DIR used by `riak start` (daemon mode) and is
# not creatable by the non-root riak user at runtime, so pre-create it here.
RUN mkdir -p /var/run/riak \
    && chown -R riak:riak /etc/riak /var/lib/riak /var/log/riak /var/run/riak

COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 755 /usr/local/bin/entrypoint.sh

# Data directory — mount a PersistentVolume here.
VOLUME ["/var/lib/riak"]

EXPOSE 8087 8098

USER riak

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
