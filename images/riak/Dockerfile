# Riak KV 3.2 container image
# RPM source: https://files.tiot.jp/riak/kv/3.2/
# Base: Red Hat Universal Base Image 8

FROM registry.access.redhat.com/ubi8/ubi AS base

ARG RIAK_VERSION=3.2.6
ARG RIAK_OTP=OTP24
ARG RIAK_ARCH=x86_64
ARG RIAK_RPM_URL=https://files.tiot.jp/riak/kv/3.2/${RIAK_VERSION}/rhel/8/riak-${RIAK_VERSION}.${RIAK_OTP}-1.el8.${RIAK_ARCH}.rpm

# Enable EPEL to satisfy the /usr/bin/escript dependency declared by the
# Riak RPM spec (provided by erlang-erts).  Riak bundles its own OTP24
# runtime; erlang-erts is only needed to satisfy the RPM dependency check.
# procps-ng: provides `ps`, used by riak startup scripts.
RUN dnf install -y \
        https://dl.fedoraproject.org/pub/epel/epel-release-latest-8.noarch.rpm \
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
