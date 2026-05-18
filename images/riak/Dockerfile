# Riak KV 3.2 container image
# RPM source: https://files.tiot.jp/riak/kv/3.2/
# Base: Red Hat Universal Base Image 9

FROM registry.access.redhat.com/ubi9/ubi AS base

ARG RIAK_VERSION=3.2.6
ARG RIAK_OTP=OTP24
ARG RIAK_ARCH=x86_64
ARG RIAK_RPM_URL=https://files.tiot.jp/riak/kv/3.2/${RIAK_VERSION}/rhel/9/riak-${RIAK_VERSION}.${RIAK_OTP}-1.el9.${RIAK_ARCH}.rpm

# Install Riak RPM directly from tiot.jp.
# The RPM bundles Erlang/OTP 24 at /usr/lib64/riak/erts-*/; no separate
# Erlang runtime is required at runtime.  However the RPM spec lists
# /usr/bin/escript as a dependency — a leftover from non-bundled builds —
# which dnf cannot satisfy on UBI9.  We download the RPM manually and
# install with --nodeps to bypass that stale spec dependency.
# procps-ng: provides `ps`, used by riak startup scripts.
RUN dnf install -y procps-ng curl \
    && curl -fsSL -o /tmp/riak.rpm "${RIAK_RPM_URL}" \
    && rpm -ivh --nodeps /tmp/riak.rpm \
    && rm /tmp/riak.rpm \
    && dnf remove -y curl \
    && dnf clean all \
    && rm -rf /var/cache/dnf

# Ensure directories are writable by the riak user created by the RPM.
# The RPM sets these up, but explicit chown guards against image layer ordering issues.
RUN chown -R riak:riak /etc/riak /var/lib/riak /var/log/riak

COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 755 /usr/local/bin/entrypoint.sh

# Data directory — mount a PersistentVolume here.
VOLUME ["/var/lib/riak"]

EXPOSE 8087 8098

USER riak

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
