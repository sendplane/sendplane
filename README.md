# sendplane

sendplane은 listmonk/Keila 계열의 임베더블 메일 발송 엔진입니다. Go 라이브러리(`sendplane.New`)로 호스트 애플리케이션에
임베딩하거나, 역할별(`control`/`sender`/`bounce`)로 나눠 배포할 수 있는 참조 바이너리로도 제공됩니다.

- 설계 문서: [docs/architecture.md](docs/architecture.md)
- 결정 기록(ADR): [docs/adr/README.md](docs/adr/README.md)
- 구현 로드맵: [docs/roadmap.md](docs/roadmap.md)
- 기여 가이드: [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md)

## 기능

- **캠페인 + transactional**: 하나의 Delivery 파이프라인, 진입점과 스케줄링 레인만 다름(architecture §1).
- **1M+ 수신자**: 연락처 DB 없이 NDJSON 스트리밍 인제스트, 청크 멱등성(ADR-0005).
- **메일 단위 재시도**: `Delivery`/`DeliveryAttempt` 상태기계, 에러 분류별 백오프(ADR-0003, architecture §4).
- **Liquid 템플릿 + i18n**: `{% t %}` 태그, 로케일 폴백 체인, HTML 기본 이스케이프(ADR-0004).
- **MJML 블록 에디터**: GrapesJS-MJML, publish 시 1회 컴파일(ADR-0009).
- **오픈/클릭 트래킹 + 수신거부**: 무상태 서명 토큰, 2단계 수신거부, 호스트 위임 가능(ADR-0011).
- **바운스 처리**: VERP + 헤더 3중 상관관계, DSN/ARF 파싱, 선택적 내장 suppression(ADR-0008).
- **루프백 발신 헬스체크**: 실제 발송 경로로 프로브 메일을 보내 SPF/DKIM/DMARC/스팸함 판정, DNS는 진단 보조(ADR-0012).
- **Postgres/MongoDB**: `store` 인터페이스 + `storetest` 적합성 스위트로 두 구현을 동일하게 검증(ADR-0007).
- **운영 콘솔**: Vue 3, 라우터 비의존 페이지 컴포넌트 + 참조 바이너리에 embed(ADR-0010).

## 패키지 맵

| 경로 | 내용 |
|---|---|
| `sendplane.go`, `options.go` | 공개 임베딩 API(`New`, `Handler`, `RunControl`, `RunSender`, `RunBounce`) |
| `host/` | 호스트가 주입/수신하는 타입(leaf 패키지, architecture §2.1) |
| `store/`, `store/postgres/`, `store/mongo/`, `store/storetest/` | 리포지터리 계약 + 두 구현 + 적합성 스위트 |
| `internal/api` | HTTP 레이어, `api/openapi.yaml`의 85개 오퍼레이션과 1:1 |
| `internal/control` | 리더 루프(스케줄러/파이널라이저/아웃박스/보존기간/…) |
| `internal/ingest`, `internal/render`, `internal/sender` | 인제스트 · 렌더링 · 발송 파이프라인 |
| `internal/bounce`, `internal/mailbox` | 바운스 파싱/상관관계, IMAP/POP3 클라이언트 |
| `internal/probe`, `internal/dnscheck` | 루프백 헬스체크, DNS 진단 계층 |
| `internal/tracking` | 서명 토큰(오픈/클릭/수신거부), VERP 주소 |
| `cmd/sendplane`, `cmd/chaos-smtp` | 참조 바이너리, 테스트용 결정적 실패 SMTP 서버 |
| `web/` | pnpm workspace: `@sendplane/api`, `@sendplane/ui`, `@sendplane/console`([web/README.md](web/README.md)) |
| `deploy/dev`, `deploy/helm` | 로컬 docker-compose, Helm 차트 |
| `test/load`, `test/e2e` | 1M 부하 테스트, 종단 간 시나리오 하니스 |

## 개발 환경 빠른 시작

```sh
make dev-up              # deploy/dev/docker-compose.yml (postgres:55441, mongo:55442)
cp .env.example .env     # SENDPLANE_TEST_* DSN 설정
make test
```

`make dev-down` 으로 로컬 의존성을 정리합니다. `make ci` 는 CI와 동일한 gen-check/fmt/vet/lint/test 체크를 로컬에서 실행합니다.

종단 간 테스트와 1M 부하 테스트도 로컬에서 그대로 돌릴 수 있습니다(둘 다 이미지를 빌드하고 자체 compose 스택을 띄운 뒤 정리까지 합니다):

```sh
make e2e                      # 시나리오 8개(부트스트랩~SIGKILL 복구). 자세한 내용: test/e2e/README.md
make e2e E2E_FLAGS=--kill-sender

make load-test                # 10만 건(로컬 기본값), --kill-sender 포함
make load-test N=1000000      # 진짜 100만 건. 자세한 내용: test/load/README.md
```

운영 콘솔(Vue, `web/apps/console`)까지 넣은 참조 바이너리를 실행하려면:

```sh
make web-install     # 최초 1회
make console-sync    # web/apps/console/dist 를 빌드해 cmd/sendplane/console/dist 로 복사(ADR-0010)
go build -o sendplane ./cmd/sendplane
cp cmd/sendplane/config.example.yaml config.yaml   # 필요한 값 채우기
./sendplane --config=config.yaml --roles=control,sender,bounce
```

`http://localhost:8080/console/` 에서 콘솔을 엽니다. `console-sync` 없이 빌드해도
`go build`는 되지만(커밋된 플레이스홀더 덕분) 콘솔은 "not built" 페이지만 보입니다.
자세한 내용은 [cmd/sendplane/README.md](cmd/sendplane/README.md#운영-콘솔-cmdsendplaneconsole)를
참고하세요.

## 상태: 개발 초기

아직 공개 API가 안정화되지 않았습니다. 구현 순서와 각 Phase의 진행 상태는 [docs/roadmap.md](docs/roadmap.md)를 참고하세요 —
핵심 파이프라인(Phase 0-5)은 끝났고, MongoDB e2e·배포·부하 테스트(Phase 7·9)는 구현은 됐지만 GitHub Actions에서의
실행 이력이 아직 없습니다.
