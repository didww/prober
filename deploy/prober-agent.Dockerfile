# Same as the backend: the static binary is built in CI and copied in.
#
# NOT the :nonroot variant. The agent opens raw ICMP sockets (CAP_NET_RAW). A
# non-root process only holds an added capability if it is in its effective set,
# which needs file capabilities the distroless image cannot set. So the agent
# runs as root with every capability dropped except NET_RAW, added by the pod's
# securityContext (or `docker run --cap-add=NET_RAW`) — the blackbox-exporter
# pattern. It still has no shell and no libc.
FROM gcr.io/distroless/static-debian13

ARG TARGETARCH
COPY dist/linux/${TARGETARCH}/prober-agent /opt/prober-agent/bin/prober-agent
COPY packaging/agent.docker.yml /opt/prober-agent/etc/config.yml

# The agent dials out; it listens on nothing.
USER 0:0

ENTRYPOINT ["/opt/prober-agent/bin/prober-agent"]
CMD ["-config", "/opt/prober-agent/etc/config.yml"]
