COMPOSE ?= docker compose
.PHONY: build gen gen-check test test-store lint vet fmt-check dev-up dev-down ci docker \
	web-install web-gen web-lint web-test web-build web-ci

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
