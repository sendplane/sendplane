# 기여 가이드

## Makefile 타겟

| 타겟 | 하는 일 |
|---|---|
| `make build` | `go build ./...` |
| `make gen` | `api/openapi.yaml` → `internal/api/gen.go` 재생성(`oapi-codegen`, `go.mod`의 `tool` 디렉티브로 고정) + `gofmt` |
| `make gen-check` | `make gen` 후 `git diff --exit-code` — 스펙과 생성물이 어긋나면 실패(CI가 씀) |
| `make test` | `go test -race ./...` |
| `make test-store` | `go test -race ./store/...` (storetest 포함, DSN 환경변수 없으면 해당 백엔드 스킵) |
| `make lint` | `golangci-lint run` |
| `make vet` | `go vet ./...` |
| `make fmt-check` | `gofmt -l .`이 비어 있는지 확인 |
| `make dev-up` / `make dev-down` | `deploy/dev/docker-compose.yml`(postgres:55441, mongo:55442) 기동/정리 |
| `make dev-reset` | `dev-down`에 볼륨 삭제까지(`-v`) — 깨끗한 DB로 다시 시작 |
| `make dev` | 원커맨드 로컬 개발 환경: DB 기동+healthy 대기+마이그레이션 후 chaos-smtp(로컬 메일 싱크)/`cmd/sendplane`(인증 꺼짐)/콘솔 dev 서버를 포그라운드로 함께 실행. `Ctrl-C`로 세 프로세스만 정리(DB는 유지). 자세한 내용: README.md의 "개발 환경 한 번에 띄우기" |
| `make dev-migrate` | `go run ./cmd/sendplane --config deploy/dev/config.yaml --migrate` |
| `make dev-server` | `deploy/dev/config.yaml`로 `cmd/sendplane` 단독 실행(`:8080`, 인증 꺼짐) |
| `make dev-web` | 콘솔 Vite dev 서버 단독 실행(`:5173`, `/api`·`/t`를 `:8080`으로 프록시) |
| `make dev-smtp` | `cmd/chaos-smtp` 단독 실행, 실패율 0인 로컬 메일 싱크(`127.0.0.1:12525`, 통계 `:12590`) |
| `make docker` | 참조 이미지(`cmd/sendplane` + `cmd/chaos-smtp`, 웹 콘솔 포함) 빌드 |
| `make ci` | `gen-check fmt-check vet lint test` — 푸시 전 로컬 체크 |
| `make e2e` | 이미지 빌드 → `test/e2e/docker-compose.yml` 기동 → `go run ./test/e2e` → 로그 덤프 → 정리. `E2E_FLAGS`로 하니스 플래그 전달(`make e2e E2E_FLAGS=--kill-sender`) |
| `make load-test` | 이미지 빌드 → `test/load/docker-compose.yml` 기동 → `go run ./test/load --recipients=$N --kill-sender` → 로그 덤프 → 정리. `N`으로 수신자 수 조절(기본 100,000; `make load-test N=1000000`이 진짜 100만 건) |
| `make web-install` | `cd web && pnpm install` |
| `make web-gen` | `cd web && pnpm gen` — `api/openapi.yaml` → `web/packages/api/src/schema.d.ts` |
| `make web-lint` | `cd web && pnpm lint && pnpm typecheck` |
| `make web-test` | `cd web && pnpm test` |
| `make web-build` | `cd web && pnpm build` — `web/apps/console/dist` 생성 |
| `make web-ci` | `web-install web-gen web-lint web-test web-build` |
| `make console-sync` | `VITE_BASE=/console/`로 콘솔을 빌드해 `cmd/sendplane/console/dist`로 복사(ADR-0010). `web-install` 선행 필요 |

## 스토어 적합성(conformance) 테스트

`store` 패키지가 정의하는 리포지터리 인터페이스는 `store/storetest`에 구현체 독립적인 스위트(54개 서브테스트)로 검증합니다.
새 스토어 구현(Postgres, MongoDB, 그 외 커스텀 구현)을 추가하거나 수정할 때는 **반드시 `storetest.Run`을 통과해야
합니다**. 테넌트 격리, 페이지네이션, 멱등 삽입/전이, claim 무중복, lease 회수 같은 계약은 각 구현이 개별적으로
재검증하지 않고 이 스위트 하나로 보장합니다(자세한 배경은 [ADR-0007](adr/0007-repository-conformance.md) 참고).
**새 리포지터리 메서드는 storetest 케이스 없이는 머지하지 않습니다.**

## 환경 변수 규칙

DB를 사용하는 conformance 테스트는 아래 두 환경 변수로 대상 DB에 접속하며, **값이 없으면 해당 백엔드 테스트를
스킵**합니다. 로컬 값은 `.env.example`을 참고하세요.

- `SENDPLANE_TEST_POSTGRES_DSN`
- `SENDPLANE_TEST_MONGO_URI`

로컬에서 두 DB를 띄우려면 `make dev-up`(`deploy/dev/docker-compose.yml`)을 사용하세요.

## OpenAPI 스펙을 고쳤다면

`api/openapi.yaml`이 Go 서버 타입과 TS 클라이언트 타입 양쪽의 단일 진실 원천입니다. 스펙을 바꾼 뒤에는 **양쪽
생성물을 모두 재생성하고 커밋**해야 합니다.

```sh
make gen       # internal/api/gen.go 재생성 (Go 서버 타입 + strict server 인터페이스 + 라우팅)
make web-gen   # web/packages/api/src/schema.d.ts 재생성 (또는 cd web && pnpm gen)
```

두 생성물 모두 CI(`ci.yml`의 `test`/`web` 잡)가 `git diff --exit-code`로 drift를 검사합니다 — 스펙만 고치고
생성물을 커밋하지 않으면 빌드가 깨집니다. 새 오퍼레이션을 추가했다면 `internal/api/actions.go`의
`operationID → host.Action` 표도 함께 채우세요(`TestEveryOperationHasAnAction`이 스펙과 대조합니다).

## 푸시 전 체크

```sh
make ci   # gen-check, fmt-check, vet, lint, test
```

## CI 워크플로우

세 워크플로우 모두 `.github/workflows/`에 있습니다.

| 워크플로우 | 트리거 |
|---|---|
| `ci.yml` | PR, `main` push. `lint`/`test`(postgres·mongo storetest 매트릭스)/`web` 3개 잡 |
| `e2e.yml` | PR, `main` push, 매일 02:40 UTC(mongo 오버레이 추가), 수동(`workflow_dispatch`) |
| `load-1m.yml` | 매일 03:40 UTC, 수동(`workflow_dispatch`, `recipients`/`kill_sender` 입력으로 조정 가능) |

`ci.yml`은 사실상 매 PR마다 돌지만, `e2e.yml`의 mongo 경로와 `load-1m.yml`은 **일상적인 PR 검증에는 끼지 않습니다** —
로컬에서 `make e2e`/`make load-test`로 직접 확인하거나 `workflow_dispatch`로 수동 실행하세요.