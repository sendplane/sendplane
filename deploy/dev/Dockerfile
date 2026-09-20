# One image, two binaries: cmd/sendplane runs any combination of the
# control/sender/bounce roles via --roles (ADR-0001), cmd/chaos-smtp is the
# deterministic-failure SMTP server used by load/e2e tests. Shipping both in
# one image means a Helm deploy and a chaos-smtp sidecar never drift apart on
# version.
#
# Build:  make docker   (docker build -f deploy/dev/Dockerfile -t sendplane:dev .)
# Run:    docker run sendplane:dev --config=/etc/sendplane/config.yaml
#         docker run --entrypoint /usr/local/bin/chaos-smtp sendplane:dev --listen=:2525

# --- build stage -------------------------------------------------------
FROM golang:1.25-bookworm AS build

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/sendplane ./cmd/sendplane && \
    go build -trimpath -ldflags="-s -w" -o /out/chaos-smtp ./cmd/chaos-smtp

# --- runtime stage -------------------------------------------------------
# distroless/static: no shell, no package manager, CGO_ENABLED=0 above makes
# that safe (pgx and the mongo driver are pure Go; net.Resolver falls back to
# the pure-Go DNS resolver without libc/nsswitch).
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/sendplane /usr/local/bin/sendplane
COPY --from=build /out/chaos-smtp /usr/local/bin/chaos-smtp

USER nonroot:nonroot
WORKDIR /

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/sendplane"]
