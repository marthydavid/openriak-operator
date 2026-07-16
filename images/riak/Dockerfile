# Riak KV container image on Red Hat Universal Base Image.
# RPM source: https://files.tiot.jp/riak/kv/
#
# Build args select the Riak release and matching RHEL base. Upstream ships
# each release for specific RHEL majors only (3.2.x: el8; 3.4.x: el9 with
# OTP26), so RHEL_VERSION and RIAK_OTP must match the chosen RIAK_VERSION —
# see .github/workflows/build-riak.yml for the published combinations.
#
#   3.2.6 (default): --build-arg RIAK_SERIES=3.2 RIAK_VERSION=3.2.6 RIAK_OTP=OTP24 RHEL_VERSION=8
#   3.4.0:           --build-arg RIAK_SERIES=3.4 RIAK_VERSION=3.4.0 RIAK_OTP=OTP26 RHEL_VERSION=9

ARG RHEL_VERSION=8

FROM registry.access.redhat.com/ubi${RHEL_VERSION}/ubi AS base

ARG RHEL_VERSION
ARG RIAK_SERIES=3.2
ARG RIAK_VERSION=3.2.6
ARG RIAK_OTP=OTP24
ARG RIAK_ARCH=x86_64
ARG RIAK_RPM_URL=https://files.tiot.jp/riak/kv/${RIAK_SERIES}/${RIAK_VERSION}/rhel/${RHEL_VERSION}/riak-${RIAK_VERSION}.${RIAK_OTP}-1.el${RHEL_VERSION}.${RIAK_ARCH}.rpm

# Enable EPEL to satisfy the /usr/bin/escript dependency declared by the
# Riak RPM spec (provided by erlang-erts).  Riak bundles its own OTP
# runtime; erlang-erts is only needed to satisfy the RPM dependency check.
# procps-ng: provides `ps`, used by riak startup scripts.
RUN dnf install -y \
        "https://dl.fedoraproject.org/pub/epel/epel-release-latest-${RHEL_VERSION}.noarch.rpm" \
    && dnf install -y \
        procps-ng \
        erlang-erts \
    && dnf install -y "${RIAK_RPM_URL}" \
    && dnf clean all \
    && rm -rf /var/cache/dnf

# Ensure directories are writable by the riak user created by the RPM.
# The RPM sets these up, but explicit chown guards against image layer ordering issues.
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
