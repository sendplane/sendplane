COMPOSE ?= docker compose
.PHONY: build test test-store lint vet fmt-check dev-up dev-down ci

build:
	go build ./...

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

ci: fmt-check vet lint test
