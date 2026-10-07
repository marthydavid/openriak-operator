# Basho Bench from the OpenRiak fork, for load-testing Riak (including mTLS clusters).
# See docs/basho-bench.md. Pinned to the commit of branch openriak-3.2 that was tested.
FROM docker.io/library/erlang:26

ARG BASHO_BENCH_REF=6108848f46a57d315c1c7aa2d6010acc8258bcf2

# erlang:26 already ships rebar3; eleveldb builds snappy with cmake.
RUN apt-get update && apt-get install -y --no-install-recommends git build-essential cmake ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN git clone https://github.com/OpenRiak/basho_bench /opt/basho_bench \
    && git -C /opt/basho_bench checkout "${BASHO_BENCH_REF}"
WORKDIR /opt/basho_bench

# The protobuf driver passes pb_connect_options (credentials, cacertfile, certfile, keyfile) to the
# Riak client but never starts the ssl application, so every TLS connection fails with
# {tcp,ssl_not_started} (silently with auto_reconnect). Start it before the first connection.
RUN sed -i '0,/^new(Id) ->/s//new(Id) ->\n    _ = application:ensure_all_started(ssl),/' src/basho_bench_driver_riakc_pb.erl \
    && grep -n -A1 '^new(Id) ->' src/basho_bench_driver_riakc_pb.erl

RUN rebar3 escriptize || (rebar3 compile && rebar3 escriptize)
RUN cp _build/default/bin/basho_bench /usr/local/bin/basho_bench

# Runs as an arbitrary non-root UID on OpenShift (group 0 owns the tree).
RUN chgrp -R 0 /opt/basho_bench && chmod -R g=u /opt/basho_bench
USER 1001
WORKDIR /opt/basho_bench
ENTRYPOINT ["/bin/bash"]
