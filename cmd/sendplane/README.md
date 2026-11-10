# cmd/sendplane

sendplane 라이브러리(`sendplane.New`)를 감싼 참조 바이너리입니다. 설정 파일 하나로 API Key/JWT 인증, webhook
이벤트 전달, AES-GCM 비밀 암호화를 구성하고, 같은 바이너리·같은 이미지로 `control`/`sender`/`bounce` 역할을
원하는 조합으로 실행합니다(ADR-0001, docs/architecture.md §2-3).

## 빌드 · 실행

```sh
go build -o sendplane ./cmd/sendplane
cp config.example.yaml config.yaml   # 필요한 값 채우기
./sendplane --config=config.yaml --roles=control,sender,bounce
```

## 플래그 / 환경변수

| 플래그 | 환경변수 | 기본값 | 설명 |
|---|---|---|---|
| `--config` | `SENDPLANE_CONFIG` | `config.yaml` | 설정 파일 경로 |
| `--roles` | `SENDPLANE_ROLES` | `control,sender,bounce` | 실행할 역할(쉼표 구분) |
| `--listen` | - | `:8080` | HTTP 서버 주소 |
| `--metrics-listen` | `SENDPLANE_METRICS_LISTEN` | `""`(끔) | Prometheus `GET /metrics`를 서빙할 **별도** HTTP 서버 주소. `--listen`과 같으면 기동 거부 |
| `--migrate` | - | `false` | 마이그레이션만 실행하고 종료 |
| `--migrate-on-start` | - | `false` | 역할을 시작하기 전에 마이그레이션 실행 |

HTTP 서버는 어떤 역할 조합이든 항상 기동되고 `/healthz`를 응답합니다(쿠버네티스 프로브용). `control`이
`--roles`에 있으면 `sp.Handler()`(REST API)가 `/`에 마운트되고 `/healthz`는 **그 핸들러가** 스펙대로 JSON으로
답합니다. control이 없는 sender/bounce 전용 파드에서만 평문 `/healthz`를 따로 등록합니다 — 둘 다 등록하면
`net/http` mux가 더 구체적인 `/healthz`를 골라 API 라우트를 가려 버립니다.

## 메트릭

`--metrics-listen`(예: `:9090`)을 주면 [`prommetrics`](../../prommetrics) 익스포터가 `Options.Metrics`로 연결되고,
**모든 역할**에서 그 주소의 별도 HTTP 서버가 `GET /metrics`만 응답합니다. 기본값이 꺼짐인 이유는 `make dev`가 한
호스트에서 역할별 프로세스 셋을 띄우기 때문입니다 — 고정 기본 포트는 서로 충돌합니다. Helm 차트는
`--metrics-listen=:9090`을 넘깁니다.

메인 리스너(`--listen`)와 포트를 나누는 것이 핵심입니다. 메인 리스너는 Ingress로 공개되는데, 메트릭에는 테넌트
ID(`sendplane_sender_claimed_total{tenant}`)가 들어 있어 공개되면 안 됩니다. 그래서 메인 mux는 `/metrics`를
등록하지 않고, 두 주소가 같은 소켓을 가리키면(`:9090`과 `0.0.0.0:9090` 포함) 기동 시 에러를 냅니다. 두 서버는
같은 errgroup에서 같은 `ReadHeaderTimeout`과 30초 드레인으로 함께 기동·종료됩니다.

| 메트릭 | 종류 | 레이블 |
|---|---|---|
| `sendplane_sender_claimed_total` | counter | `lane`, `tenant` |
| `sendplane_sender_processed_total` | counter | `status`, `class` |
| `sendplane_sender_render_seconds` | histogram | - |
| `sendplane_sender_smtp_seconds` | histogram | `transport`, `class` |
| `sendplane_sender_limiter_wait_seconds` | histogram | `transport` |
| `sendplane_sender_conn_opened_total` | counter | `transport` |
| `sendplane_sender_conn_closed_total` | counter | - |
| `sendplane_sender_transport_status_total` | counter | `transport`, `status` |
| `sendplane_bounce_handled_total` | counter | `type`, `status` |
| `sendplane_bounce_skipped_total` | counter | `reason` |
| `sendplane_bounce_poll_errors_total` | counter | `phase` |
| `sendplane_bounce_platform_suppressed_total` | counter | `reason` |
| `sendplane_delivery_queue_depth` | gauge | `lane`, `status` |
| `sendplane_delivery_queue_depth_errors_total` | counter | - |
| `sendplane_metrics_dropped_total` | counter | `metric` |

여기에 Go 런타임(`go_*`)과 프로세스(`process_*`) 메트릭이 더해집니다. 히스토그램은 모두 초 단위입니다.

- `sendplane_delivery_queue_depth`는 **control 역할에서만** 등록됩니다. 전 테넌트 합계의 비종료 상태
  (pending/queued/leased/deferred) 건수를 store에서 읽어 15초간 캐시하며, 3개 레인 × 4개 상태가 0이라도 항상
  노출됩니다. control 레플리카마다 **같은 클러스터 전체 값**을 내보내므로 대시보드·알림·KEDA에서는 레플리카
  간에 `sum`이 아니라 `max`로 집계해야 합니다. store가 지원하지 않으면(`errors.ErrUnsupported`) 이 게이지는
  나오지 않고, 읽기 실패 시에는 마지막 정상값을 최대 75초(5 × 15초)까지 유지하며 `_errors_total`이 증가합니다.
- `sendplane_metrics_dropped_total`은 익스포터가 거부한 샘플 수입니다(잘못된 이름·레이블, 첫 호출과 다른
  레이블 키 집합, 음수 증가분, counter/histogram 혼용 등). 0이 아니면 코드 버그이며, 이름별로 한 번 경고 로그가
  남습니다.

## 설정 파일

전체 스키마는 [config.example.yaml](config.example.yaml)을 참고하세요. `${VAR}` 형태의 값은 프로세스
환경변수로 치환된 뒤 파싱됩니다(`os.ExpandEnv`) — Helm 차트가 Secret의 DSN/키를 ConfigMap 없이 주입하는
방식이 바로 이것입니다.

- `store`: `driver`(postgres|mongo), `dsn`, `mongo_db`
- `secrets.key`: AES-256-GCM용 32바이트, base64. `openssl rand -base64 32`로 생성
- `auth.mode`: `apikey`(해시된 키 목록) · `jwt`(JWKS 검증, 캐시/자동 갱신) · `none`(로컬 개발 전용, 기동 시
  경고 로그)
- `authz.mode`: `allow_all` 또는 `roles`(역할별 glob 패턴을 `host.Action` 문자열에 매칭)
- `events.webhook`: HMAC-SHA256 서명(`X-Sendplane-Signature`) webhook. SSRF 방지를 위해 기본적으로
  사설/루프백 대역으로의 연결을 거부합니다(`allow_private_networks`로 해제)
- `sender`: 레인별 워커 수, claim 배치 크기, 리스 시간 등 (`docs/architecture.md` §8). `lanes.probe`는 최소 1이어야
  루프백 프로브가 실제로 나갑니다
- `bounce`: 메일박스별 lock owner(`worker_id`), 폴링/새로고침 주기, IMAP IDLE 사용 여부 (§10).
  메일박스 자체는 테넌트 리소스입니다: `POST /api/v1/bounce-mailboxes`
- `probe`: 루프백 헬스 체크 (§11). `hmac_key`(base64)가 있으면 켜지고 `enabled`로 명시적으로 덮을 수 있습니다.
  `nameservers`는 DNS 진단 계층이 직접 질의할 리졸버입니다
- `limits`: `host.Limits`에 매핑, 0인 필드는 기본값으로 채워집니다

## 운영 콘솔 (cmd/sendplane/console)

`control`이 `--roles`에 있으면 `config.yaml`의 `console.path`(기본 `/console`, "/"가
아닌 이유는 아래 참고)에 Vue 운영 콘솔(`web/apps/console`)이 함께 마운트됩니다.
`console.enabled: false`로 끌 수 있습니다.

콘솔은 [`cmd/sendplane/console`](console) 패키지가 `//go:embed dist/*`로 통째로
바이너리에 넣습니다(ADR-0010). `cmd/sendplane/console/dist/`는 `index.html`
플레이스홀더 한 장만 커밋되어 있고("console not built; run make web-build
console-sync") 나머지는 `.gitignore` 대상이라, Node 없이도 클론 직후
`go build ./cmd/sendplane`가 됩니다. 진짜 콘솔을 넣으려면:

```sh
make web-install    # 최초 1회, web/ 에 pnpm install
make console-sync   # VITE_BASE=/console/ 로 web/apps/console/dist 를 빌드해
                     # cmd/sendplane/console/dist/ 로 복사
go build -o sendplane ./cmd/sendplane
./sendplane --config=config.yaml --roles=control,sender,bounce
# http://localhost:8080/console/
```

`console.path`를 바꾸면 `VITE_BASE`도 그 경로(끝에 `/`)로 맞춰 `console-sync`를
다시 돌려야 합니다 — 콘솔 자산의 URL이 빌드 시점에 base path로 고정되기 때문입니다.

경로가 `/`가 아니라 `/console`인 이유는 `sp.Handler()`가 이미 `/api/v1`(REST
API)과 `/t/`(트래킹)를 `/` 아래에 소유하고 있어서입니다. `/console`은 `net/http`의
`ServeMux`가 `/`보다 더 구체적인 패턴으로 우선 매칭해 주는, 겹치지 않는 자기
자리입니다. `cmd/sendplane/console.Handler`는 알 수 없는 하위 경로(클라이언트 사이드
라우팅)를 전부 `index.html`로 폴백하고, 해시가 붙은 `assets/*`는 영구 캐시,
`index.html` 자신은 `no-store`로 응답합니다.

콘솔의 API 주소는 기본적으로 same-origin(`/api/v1`)입니다 — 별도 설정 없이 같은
`sendplane` 프로세스에 붙습니다. 자세한 프론트엔드 구성은
[`web/README.md`](../../web/README.md)를 참고하세요.

## cmd/chaos-smtp

`internal/chaossmtp`(결정적으로 실패하는 테스트용 SMTP 서버)를 감싼 얇은 바이너리입니다.
`--tempfail/--permfail/--drop/--latency/--seed/--ratelimit-after`로 실패율을 설정하고,
`--stats-listen`(기본 `:9090`)의 `GET /stats`/`POST /reset`으로 상태를 조회·초기화합니다.
