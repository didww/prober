# The CI job cross-compiles the static binary first (with the SPA already built
# and embedded) and drops it in dist/linux/<arch>/, so there is no build stage
# here and no toolchain in the image.
#
# Debian 13 (trixie) distroless static, pinned to the major rather than a
# floating tag so the base cannot move under two builds of the same version.
# ca-certificates ships in it, which OIDC discovery over HTTPS needs; nonroot
# out of the box.
FROM gcr.io/distroless/static-debian13:nonroot

ARG TARGETARCH
COPY dist/linux/${TARGETARCH}/prober-backend /opt/prober-backend/bin/prober-backend
COPY packaging/backend.docker.yml /opt/prober-backend/etc/config.yml

# 8080 the browser UI, 50051 the agent gRPC gateway, 9108 the metrics exporter.
EXPOSE 8080 50051 9108
USER nonroot:nonroot

ENTRYPOINT ["/opt/prober-backend/bin/prober-backend"]
CMD ["-config", "/opt/prober-backend/etc/config.yml"]
