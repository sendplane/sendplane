COMPOSE ?= docker compose
.PHONY: build gen gen-check test test-store lint vet fmt-check dev-up dev-down ci docker load-test \
	web-install web-gen web-lint web-test web-build web-ci console-sync \
	dev dev-web-deps dev-wait-db dev-migrate dev-server dev-web dev-smtp dev-reset

build:
	go build ./...

# internal/api/gen.go is generated from api/openapi.yaml, which is the single
# source of truth (architecture 15). The generator is pinned by the `tool`
# directive in go.mod, so this is reproducible without installing anything.
gen:
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	gofmt -w internal/api/gen.go

# CI fails on drift between the spec and the checked-in generated code.
gen-check: gen
	@git diff --exit-code -- internal/api/gen.go || \
		(echo "internal/api/gen.go is stale; run make gen and commit the result"; exit 1)

test:
	go test -race ./...

# storetest conformance suite; reads SENDPLANE_TEST_POSTGRES_DSN /
# SENDPLANE_TEST_MONGO_URI from the environment (see .env.example) and
# skips the corresponding backend when the var is unset.
test-store:
	go test -race ./store/...

lint:
	golangci-lint run

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needs to be run on:"; gofmt -l .; exit 1)

dev-up:
	$(COMPOSE) -f deploy/dev/docker-compose.yml up -d

dev-down:
	$(COMPOSE) -f deploy/dev/docker-compose.yml down

# Like dev-down, but also removes the postgres/mongo volumes - a clean
# database, for when leftover data from an earlier `make dev`/`make e2e`
# session gets in the way.
dev-reset:
	$(COMPOSE) -f deploy/dev/docker-compose.yml down -v

# --- make dev: one-command local dev environment ---
#
# `make dev` brings up postgres/mongo, migrates the schema, and runs three
# long-lived processes together in the foreground via deploy/dev/dev.sh:
# cmd/chaos-smtp as a local mail sink, the sendplane server with
# authentication disabled (deploy/dev/config.yaml, auth.mode: none - see that
# file for why that's fine here and nowhere else) and the console's Vite dev
# server (hot reload, proxying to the server - web/apps/console/vite.config.ts).
# Ctrl-C stops those three; the DB containers keep running (`make dev-down`
# stops those, `make dev-reset` also wipes them).
#
# Each piece is also runnable on its own, in its own terminal, without going
# through dev.sh at all: `make dev-smtp`, `make dev-server`, `make dev-web`
# (after `make dev-up dev-wait-db dev-migrate` once).
dev: dev-web-deps dev-up dev-wait-db dev-migrate
	@echo ""
	@echo "sendplane dev environment:"
	@echo "  console (Vite, hot reload):   http://localhost:5173"
	@echo "  API:                          http://localhost:8080/api/v1"
	@echo "  healthz:                      http://localhost:8080/healthz"
	@echo "  embedded console (needs make console-sync first): http://localhost:8080/console/"
	@echo "  chaos-smtp stats (mail sink): http://localhost:12590/stats"
	@echo "  auth is disabled (auth.mode: none) - every request is one fixed local principal"
	@echo "  Ctrl-C stops chaos-smtp/sendplane/console; DB containers keep running (make dev-down)"
	@echo ""
	PNPM=$(PNPM) ./deploy/dev/dev.sh

# `go run ./cmd/sendplane` needs web/node_modules only through dev-web
# (pnpm --filter), but dev would otherwise fail confusingly partway through
# dev.sh instead of failing fast with a familiar `make web-install`.
dev-web-deps:
	@test -d web/node_modules || $(MAKE) web-install

# Polls the postgres container's own healthcheck (deploy/dev/docker-compose.yml)
# until docker reports it healthy or 60s pass, so dev-migrate/dev-server never
# race a postgres that's still starting up right after dev-up.
dev-wait-db:
	@cid="$$($(COMPOSE) -f deploy/dev/docker-compose.yml ps -q postgres)"; \
	if [ -z "$$cid" ]; then \
		echo "dev-wait-db: no postgres container found (did dev-up run?)" >&2; \
		exit 1; \
	fi; \
	echo "dev-wait-db: waiting for postgres ($$cid) to become healthy..."; \
	status=unknown; \
	for _ in $$(seq 1 60); do \
		status=$$(docker inspect -f '{{.State.Health.Status}}' "$$cid" 2>/dev/null || echo unknown); \
		if [ "$$status" = "healthy" ]; then \
			echo "dev-wait-db: postgres is healthy"; \
			exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "dev-wait-db: postgres did not become healthy within 60s (last status: $$status)" >&2; \
	exit 1

# Runs cmd/sendplane's own --migrate against deploy/dev/config.yaml (the
# postgres store deploy/dev/docker-compose.yml runs on :55441).
dev-migrate:
	go run ./cmd/sendplane --config deploy/dev/config.yaml --migrate

# The reference binary against deploy/dev/config.yaml, all three roles,
# authentication disabled. Foreground; Ctrl-C stops it. Runnable on its own,
# once `make dev-up dev-wait-db dev-migrate` has run at least once.
dev-server:
	go run ./cmd/sendplane --config deploy/dev/config.yaml --roles control,sender,bounce --listen :8080

# Vite dev server for the console (hot module reload), proxying /api and /t
# to :8080 - dev-server above - when VITE_SENDPLANE_API is unset
# (web/apps/console/vite.config.ts). Runnable on its own once `make
# web-install` has run.
dev-web:
	cd web && $(PNPM) --filter @sendplane/console dev

# A local mail sink so sends made against dev-server actually complete:
# point a transport at 127.0.0.1:12525 (tls: none) and read what arrived back
# from :12590 (GET /stats, GET /messages?body=1). Zero failure rates and
# unlimited retained messages+bodies, unlike cmd/chaos-smtp's own defaults
# (127.0.0.1:2525 / :9090, tuned for test/e2e and test/load instead) - the
# different ports also mean `make dev` can run alongside those.
dev-smtp:
	go run ./cmd/chaos-smtp --listen 127.0.0.1:12525 --stats-listen :12590 \
		--tempfail=0 --permfail=0 --drop=0 --keep-messages=-1 --keep-bodies

# Builds the single reference image (cmd/sendplane + cmd/chaos-smtp;
# deploy/dev/Dockerfile) that deploy/helm/sendplane deploys.
docker:
	docker build -f deploy/dev/Dockerfile -t sendplane:dev .

ci: gen-check fmt-check vet lint test

# --- web/ (pnpm workspace: @sendplane/api, @sendplane/ui, @sendplane/console) --

PNPM ?= pnpm

web-install:
	cd web && $(PNPM) install

# Regenerates web/packages/api/src/schema.d.ts from api/openapi.yaml, the same
# source of truth the Go server code is generated from. The output is committed
# and CI fails on drift (ADR-0010).
web-gen:
	cd web && $(PNPM) gen

web-lint:
	cd web && $(PNPM) lint && $(PNPM) typecheck

web-test:
	cd web && $(PNPM) test

# Produces web/apps/console/dist, which the reference binary embeds.
web-build:
	cd web && $(PNPM) build

web-ci: web-install web-gen web-lint web-test web-build

# Builds the console with the base path cmd/sendplane mounts it at by default
# (console.path in config.yaml, "/console" — see cmd/sendplane/README.md) and
# copies the result into cmd/sendplane/console/dist, which its //go:embed
# picks up (ADR-0010). Needs `make web-install` first; go build/test work
# without ever running this (see the committed dist/index.html placeholder).
console-sync:
	cd web && VITE_BASE=/console/ $(PNPM) build
	rm -rf cmd/sendplane/console/dist
	mkdir -p cmd/sendplane/console/dist
	cp -R web/apps/console/dist/. cmd/sendplane/console/dist/

# 1M-recipient load test (docs/architecture.md 15.1). N overrides the recipient
# count, which defaults to 100k so that a local run finishes in minutes; the
# nightly workflow (.github/workflows/load-1m.yml) runs the full million.
#
#   make load-test            # 100,000 recipients
#   make load-test N=1000000  # the real thing
load-test:
	docker build -f deploy/dev/Dockerfile -t sendplane:dev .
	$(COMPOSE) -f test/load/docker-compose.yml up -d
	go run ./test/load --recipients=$${N:-100000} --kill-sender; \
		status=$$?; \
		$(COMPOSE) -f test/load/docker-compose.yml logs --tail 50; \
		$(COMPOSE) -f test/load/docker-compose.yml down -v; \
		exit $$status

# End-to-end test (docs/architecture.md 15). Builds the image, brings up the
# stack in test/e2e/docker-compose.yml, runs the scenario harness, dumps the
# container logs either way and tears the stack down.
#
#   make e2e
#   make e2e E2E_FLAGS=--kill-sender          # + the lease recovery scenario
#   make e2e E2E_FLAGS="--only=4,5 --strict"  # a couple of scenarios, strictly
#
# On a machine with only the standalone Compose binary:
#   make e2e COMPOSE=docker-compose E2E_FLAGS=--compose-cmd=docker-compose
.PHONY: e2e
e2e:
	docker build -f deploy/dev/Dockerfile -t sendplane:dev .
	$(COMPOSE) -f test/e2e/docker-compose.yml up -d
	go run ./test/e2e $(E2E_FLAGS); \
		status=$$?; \
		$(COMPOSE) -f test/e2e/docker-compose.yml logs --tail 200; \
		$(COMPOSE) -f test/e2e/docker-compose.yml down -v; \
		exit $$status
