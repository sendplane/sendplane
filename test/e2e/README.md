# test/e2e

`docs/architecture.md` §15 의 **종단 간(end-to-end) 테스트**입니다.
`test/e2e/docker-compose.yml` 스택(postgres + control 1 + sender 2 + bounce 1 + chaos-smtp 1 + GreenMail 1)을
띄우고, `go run ./test/e2e` 가 시나리오 8개를 순서대로 돌리며 각각 **PASS / FAIL / KNOWN-FAIL** 을 시간과 함께 찍습니다.

한 시나리오는 첫 실패에서 멈추지 않고 **단언 실패를 모두 모아서** 보고합니다 — compose 스택 한 번 띄우는 비용이
"한 번에 차이 하나"를 감당할 만큼 싸지 않습니다.

## 돌리기

```sh
make e2e                              # 이미지 빌드 → up → 시나리오 → 로그 → down
make e2e E2E_FLAGS=--kill-sender      # 시나리오 8(lease 회수)까지
```

직접:

```sh
docker build -f deploy/dev/Dockerfile -t sendplane:dev .
docker compose -f test/e2e/docker-compose.yml up -d
go run ./test/e2e
docker compose -f test/e2e/docker-compose.yml down -v
```

호스트 포트는 `deploy/dev/docker-compose.yml`(55441/55442)·`test/load/docker-compose.yml`(15432/18080/19090)과
겹치지 않게 **postgres 15532, control 18180, chaos-smtp stats 19190, GreenMail 13325/13443/18581** 을 씁니다.
전부 환경변수로 바꿀 수 있고(`E2E_PG_PORT` 등), 바꾸면 하니스 플래그도 같이 넘겨야 합니다.

```sh
E2E_API_PORT=18190 docker compose -f test/e2e/docker-compose.yml up -d
go run ./test/e2e --api=http://127.0.0.1:18190
```

Compose 플러그인(`docker compose`) 대신 단독 바이너리만 있는 머신에서는 `--compose-cmd=docker-compose` 를
넘겨야 `--kill-sender` 단계가 동작합니다.

주요 플래그(전체는 `go run ./test/e2e -h`):

| 플래그 | 기본값 | 의미 |
|---|---|---|
| `--recipients` / `--chunk` | 10000 / 5000 | 시나리오 2의 수신자 수와 NDJSON 청크 크기 |
| `--seed` `--tempfail` `--permfail` `--drop` | 42 / 0.05 / 0.01 / 0.005 | **compose 파일의 chaos-smtp 설정과 반드시 같아야** 기대값이 맞습니다 |
| `--max-attempts` | 4 | 테넌트 재시도 정책. 런 시작 시 테넌트 설정에 그대로 씁니다 |
| `--budget` | 10m | 전체 예산. 초과하면 런 실패 |
| `--probe-timeout` | 3m | 시나리오 6이 프로브 판정을 기다리는 시간. `BUG-2` 가 살아 있는 동안은 이 시간을 통째로 기다렸다가 KNOWN-FAIL 이 됩니다 |
| `--kill-sender` | false | 시나리오 2 진행 중 sender 한 대를 SIGKILL 후 재기동(시나리오 8) |
| `--only` | (전체) | `--only=4,5` 처럼 시나리오 번호 선택. 1(부트스트랩)은 항상 돕니다 |
| `--strict` | false | KNOWN-FAIL 도 실패로 취급 |

## 스택 구성

| 서비스 | 역할 |
|---|---|
| `postgres` | 스토어. nightly 에는 `docker-compose.mongo.yml` 오버레이로 mongo 로 바뀝니다 |
| `migrate` | 마이그레이션 1회 실행 후 종료 |
| `control` | API + 리더 루프(스케줄러/파이널라이저/아웃박스/프로브 트리거·수집/lease 회수) |
| `sender` ×2 | `transactional 8 / bulk 32 / probe 1` 레인 |
| `bounce` | 바운스 메일박스 폴러(5초 간격, IDLE 끔) |
| `chaos-smtp` | 결정적 실패 SMTP(시나리오 2). `--keep-messages=-1` |
| `greenmail` | 테스트 IMAP/SMTP. 계정은 **배달·로그인 시 자동 생성** |

하니스 자신은 호스트에서 돌면서 **웹훅 수신기**(기본 `0.0.0.0:18585`)를 띄웁니다. control 컨테이너는
`extra_hosts: host.docker.internal:host-gateway` 로 그 주소에 닿고, 그래서 `config.yaml` 의
`events.webhook.allow_private_networks` 가 켜져 있습니다(시나리오 7이 검사하는 게 바로 그 경로입니다).

### GreenMail 에 대해 알아 둘 것

- 이미지 태그는 **`greenmail/standalone:2.1.14`** 로 고정합니다. SMTP 3025 / IMAP 3143 / REST API 8080.
- `-Dgreenmail.users=` 를 **일부러 쓰지 않습니다.** `bounce:pw@sendplane.test` 로 미리 만들면 그 계정의
  **로그인 아이디가 `bounce`** 가 되고, 이후 `bounce@sendplane.test` 로 IMAP 로그인하면 주소가 이미 쓰였다는
  이유로 거절됩니다. 아무것도 만들지 않으면 배달·로그인 시점에 **로그인 아이디 = 전체 주소**로 자동 생성됩니다.
- `-Dgreenmail.auth.disabled` 라서 저장된 메일박스 비밀번호는 검사되지 않습니다. 그래도 `SecretCipher`
  왕복은 그대로 일어나고, 그 부분이 검사 가치가 있는 쪽입니다.
- 하니스는 **REST API**(`GET /api/user/{주소}/messages`)로 메일을 읽고, sendplane 은 같은 메일박스를
  **진짜 IMAP** 으로 읽습니다. 문을 일부러 다르게 쓴 것입니다 — 하니스가 먼저 메일을 소비해 버리면
  IMAP 경로의 버그를 가려 버립니다.
- GreenMail 은 봉투 발신자를 `Return-Path` 헤더로 남깁니다. 시나리오 5가 **실제 VERP 주소**를
  재구성하지 않고 그대로 읽어 쓸 수 있는 이유입니다(§10).
- **`MAIL FROM:<>` 을 못 씁니다.** GreenMail 2.1.14 는 널 리버스 패스에 `250 OK` 를 답하고도 기록하지
  않아서 다음 `RCPT` 가 `503 MAIL must come before RCPT` 로 거절됩니다. RFC 3464 는 DSN 을 `<>` 로
  보내라고 하지만, 하니스는 봉투 발신자로 `MAILER-DAEMON@mx.sendplane.test` 를 씁니다. 파서가 읽는 것은
  IMAP 으로 받은 **메시지의 `Return-Path: <>` 헤더**이고 봉투가 아니므로 검사 내용은 그대로입니다.

## 시나리오

| # | 이름 | 하는 일 |
|---|---|---|
| 1 | bootstrap | `/healthz`·GreenMail·chaos-smtp 대기 → 테넌트 설정(트래킹 on/도메인 `t.e2e.test`, `unsubscribe_mode=sendplane`, suppression on, 짧은 백오프) → transport A(chaos)·B(greenmail), sending domain(VERP), sender A·B → 바운스/프로브 메일박스 → i18n(en/ko) MJML 템플릿 publish |
| 2 | bulk campaign | 10,000 수신자(en/ko 혼합)를 NDJSON 2청크로 인제스트 → start → completed. `sent`/`failed` 가 `chaossmtp.Decide` 로 유도한 값과 **정확히** 일치, 진행 중 상태 0, chaos-smtp `Accepted == sent`, 실패 표본이 전부 `max_attempts` 소진 또는 permanent |
| 3 | transactional | `POST /messages` 2명 + 커스텀 헤더 → 둘 다 `sent`, 헤더가 실제 메일에 실림, 같은 `Idempotency-Key` 재전송이 **같은 delivery id** 를 돌려주고 메일은 다시 안 나감 |
| 4 | tracking | 4명(en 2 / ko 2) 캠페인 → **ko 수신자가 ko 제목을 받았는지** 확인 → 렌더된 메일에서 픽셀/클릭/수신거부 URL 추출 → 픽셀 200 gif(`no-store`), 클릭 302(서명된 목적지), 수신거부 GET 302(호스트 목적지), 원클릭 POST 200, 위조 토큰 400 → `POST /campaigns/{id}/unsubscribes` 로 두 번째 수신자 수신거부 → delivery의 `first_opened_at`/`first_clicked_at`/`unsubscribed_at`, `GET /campaigns/{id}/links` 의 `unique_clicks`, 캠페인 통계 |
| 5 | bounce | sender B로 3통 발송 → GreenMail에서 실제 `Return-Path`(VERP)·`X-Sendplane-ID`·`Message-ID` 를 읽어 **DSN/ARF/위조 DSN을 합성** → GreenMail SMTP로 `bounce@sendplane.test` 에 투입 → `bounced` / `complained` / (위조는) `sent` 유지 + `verified=false` 이벤트, suppression 등록, 그 주소로의 재발송이 `suppressed` |
| 6 | loopback probe | `POST /senders/{B}/probe` → 프로브 메일이 `probe@sendplane.test` 에 도착(+`X-Sendplane-Probe` 헤더, 제목의 run id) → 수집 루프가 판정 → `delivered=true`, `folder=inbox`, **`yellow`** (아래 참고), sender health 가 같은 값. **지금은 `BUG-2` 때문에 수집 단계가 KNOWN-FAIL 입니다** — 벌크 캠페인이 끝난 뒤(테넌트가 한가할 때) 일부러 트리거합니다 |
| 7 | events | 웹훅 수신기에 `campaign.started`·`campaign.completed`·`delivery.bounced`·`delivery.complained`·`recipient.unsubscribed` 가 **HMAC 서명이 맞는 상태로** 도착, `GET /events/dead-letter` 와 `GET /events?status=failed` 가 비어 있음. `sender.health_changed` 는 시나리오 6의 프로브가 실제로 수집됐을 때만 요구합니다(`BUG-2`) |
| 8 | lease recovery | `--kill-sender` 일 때 시나리오 2 진행 중 sender 한 대를 SIGKILL 후 재기동. `sent`/`failed` 는 그대로 정확하고 중복은 chaos-smtp `Accepted` 에만 나타남 |

### 왜 프로브 판정이 green 이 아니라 yellow 인가

GreenMail 은 메일을 INBOX 에 넣어 주지만 **`Authentication-Results` 헤더를 붙이지 않습니다.**
`internal/probe/verdict.go` 는 신뢰할 수 있는 AR 헤더가 없으면 `yellow` 에 이유
`신뢰할 수 있는 Authentication-Results 헤더가 없습니다` 를 답합니다 — 그게 **근거 없이 green 을 주지 않는다**는
ADR-0012 의 핵심이고, 시나리오 6은 그 문자열까지 그대로 단언합니다. green 을 보려면 AR 을 붙이는 MTA
(Postfix+OpenDKIM/OpenDMARC)를 스택에 넣어야 하고, 그건 이 테스트의 범위가 아닙니다.

### 왜 i18n 제목 확인이 시나리오 2가 아니라 4에 있는가

`docs/architecture.md` §15 는 "chaos-smtp 가 기록한 메시지를 본다"를 전제하지만,
`cmd/chaos-smtp` 는 **카운터만** HTTP 로 노출합니다(`GET /stats`). `chaossmtp.Server.Messages()` 를 돌려주는
엔드포인트가 없어서 `--keep-messages=-1` 로 기록해 둔 메시지를 밖에서 읽을 방법이 지금은 없습니다.
그래서 "렌더된 메일"이 필요한 단언(로케일 제목, 트래킹 URL)은 **GreenMail 로 보내는 작은 캠페인**에서 합니다.
`--keep-messages=-1` 은 compose 에 그대로 남겨 뒀습니다 — `cmd/chaos-smtp` 에 `/messages` 가 생기면
그날 바로 쓸 수 있습니다(아래 GAP-1).

## 알려진 실패 (KNOWN-FAIL)

하니스가 `KNOWN-FAIL` 로 찍는 것은 **단언이 옳고 제품이 아직 그렇지 않은** 경우입니다. 단언은 그대로 두고
런은 실패시키지 않습니다 — 버그가 고쳐지면 그 줄이 저절로 초록이 됩니다. `--strict` 로 실패 처리할 수 있습니다.

| 태그 | 증상 | 원인 |
|---|---|---|
| `BUG-1` | 캠페인이 `completed` 된 뒤에 들어온 오픈/클릭/수신거부가 `CampaignStats.unique_opens` 등에 **영원히 반영되지 않음** | `internal/control/campaign_loops.go:81` 의 파이널라이저가 `store.CampaignRunning` 캠페인만 순회하고, `:121-137` 에서 완료 시점 통계를 한 번 쓴 뒤 상태를 `completed` 로 바꿉니다. 이후 어떤 루프도 그 캠페인의 통계를 다시 계산하지 않고 `internal/api/campaigns.go:191` 은 캐시된 행을 그대로 돌려줍니다. 수신자가 메일을 여는 시점은 **거의 항상 캠페인 완료 후**이므로 §9.3 의 "캠페인 수신거부율 = unsubscribed 유니크 / sent" 는 실질적으로 항상 0입니다. delivery 행의 `first_opened_at`/`first_clicked_at`/`unsubscribed_at` 과 `GET /campaigns/{id}/links` 는 정상이라 기록 자체는 남습니다 |
| `BUG-2` | 한가한 테넌트의 **루프백 프로브가 영원히 `pending`** 으로 남음. 판정도, 타임아웃(red)도, `sender.health_changed` 이벤트도 나오지 않음 | 모든 control 리더 루프는 `internal/control/loops.go:94` 의 `provider.ActiveTenants(ctx)` 가 돌려준 테넌트만 틱합니다. `store/postgres/postgres.go:121` 의 정의상 "비종단 delivery 또는 미완료 campaign 이 있는 테넌트"이고, `probe.go:196-205` 가 `control.WithLoop` 으로 등록하는 `probe-trigger`/`probe-collect` 에는 `internal/control/options.go` 의 `WithLoop` 이 `linger` 를 주지 않습니다(`loops.go:104`, 유예가 있는 루프는 outbox 하나뿐 — `control.go:138`). 프로브 delivery 는 1~2초 만에 `sent`(종단)가 되므로 그 직후 테넌트는 비활성이 되고, 1분 뒤 도는 `probe-collect` 는 그 테넌트를 아예 보지 않습니다. `internal/bounce` 는 같은 "일이 끝난 한참 뒤에 도착한다" 성질 때문에 일부러 `Provider.Tenants` 를 쓰는데(README) 프로브에는 그 처리가 없습니다. 결과적으로 §11 의 6시간 주기 헬스 체크는 **테넌트가 마침 다른 일을 하고 있을 때만** 완료됩니다 |

## 테스트 인프라 갭 (제품 버그 아님)

| 태그 | 내용 |
|---|---|
| `GAP-1` | `cmd/chaos-smtp` 가 `chaossmtp.Server.Messages()` 를 노출하지 않습니다. `--keep-messages` 로 기록은 되지만 읽을 방법이 없어, 렌더 결과 검증은 GreenMail 경로로 우회했습니다 |
| `GAP-2` | `delivery.sent` / `delivery.failed` 는 `api/openapi.yaml` 의 `OutboxEvent` 설명에 있지만 아웃박스에 쓰는 코드가 없습니다. 시나리오 7은 실제로 발행되는 이벤트만 단언합니다 |

## CI

[`.github/workflows/e2e.yml`](../../.github/workflows/e2e.yml) 가 PR 과 main push 에서 postgres 로,
매일 02:40 UTC 에 postgres + mongo 로 돌립니다(§15 의 "mongo 는 nightly"). 잡 타임아웃은 20분이고,
하니스의 `--budget`(기본 10분)이 그보다 먼저, 더 자세한 이유와 함께 실패합니다.
로그는 성공·실패와 무관하게 항상 덤프합니다.
