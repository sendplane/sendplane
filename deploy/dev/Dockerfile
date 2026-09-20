# One image, two binaries: cmd/sendplane runs any combination of the
# control/sender/bounce roles via --roles (ADR-0001), cmd/chaos-smtp is the
# deterministic-failure SMTP server used by load/e2e tests. Shipping both in
# one image means a Helm deploy and a chaos-smtp sidecar never drift apart on
# version. The image also ships the Vue ops console, built here and embedded
# into cmd/sendplane (ADR-0010); it answers under /console when the control
# role is running.
#
# Build:  make docker   (docker build -f deploy/dev/Dockerfile -t sendplane:dev .)
# Run:    docker run sendplane:dev --config=/etc/sendplane/config.yaml
#         docker run --entrypoint /usr/local/bin/chaos-smtp sendplane:dev --listen=:2525
#         # console at http://<host>:8080/console/ once the control role is up

# --- web build stage -----------------------------------------------------
# Builds the ops console SPA (web/apps/console/dist) that cmd/sendplane
# embeds via cmd/sendplane/console (ADR-0010). VITE_BASE matches the default
# console.path cmd/sendplane mounts it at (see cmd/sendplane/README.md); a
# custom console.path needs a matching VITE_BASE at build time.
FROM node:22-alpine AS web-build

WORKDIR /web

# web/package.json pins the exact pnpm version via "packageManager"; corepack
# reads that and installs it, so this stays reproducible without a second
# place to bump the version.
RUN corepack enable

COPY web/ .
RUN corepack prepare --activate && pnpm install --frozen-lockfile

ENV VITE_BASE=/console/
RUN pnpm build

# --- build stage -------------------------------------------------------
FROM golang:1.25-bookworm AS build

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Overwrites the committed dist/index.html placeholder (see
# cmd/sendplane/console/dist/index.html) with the real build, so the image's
# go:embed picks up the actual console instead of the "not built" page.
COPY --from=web-build /web/apps/console/dist ./cmd/sendplane/console/dist

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
