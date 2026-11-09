# Release image published to ghcr.io by the `docker` job in
# .github/workflows/ci.yml on a v* tag push.
#
# Unlike deploy/dev/Dockerfile this does not build anything: the `web` job
# cross-compiles cmd/sendplane (with the console embedded, ADR-0010) and
# cmd/chaos-smtp for every target platform and lays them out as
#
#   <context>/linux/amd64/{sendplane,chaos-smtp}
#   <context>/linux/arm64/{sendplane,chaos-smtp}
#
# and buildx picks the directory matching TARGETPLATFORM. With no RUN step the
# arm64 variant needs no QEMU emulation on the amd64 runner. --chmod because
# the binaries arrive through a workflow artifact, which drops the executable
# bit.
#
# Same layout as deploy/dev/Dockerfile's runtime stage, so both images behave
# identically (one image, two binaries; ENTRYPOINT is sendplane).
FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETPLATFORM

COPY --chmod=0755 ${TARGETPLATFORM}/sendplane /usr/local/bin/sendplane
COPY --chmod=0755 ${TARGETPLATFORM}/chaos-smtp /usr/local/bin/chaos-smtp

USER nonroot:nonroot
WORKDIR /

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/sendplane"]
