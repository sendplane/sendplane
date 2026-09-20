# internal/api

`api/openapi.yaml`의 85개 오퍼레이션을 구현하는 HTTP 레이어입니다.
설계 근거는 [architecture.md §3, §7.1, §9.1, §9.3, §12, §16](../../docs/architecture.md).

## 파일 배치

| 파일 | 내용 |
|---|---|
| `gen.go` | **생성물.** 모델 · strict server 인터페이스 · chi 라우팅. 손대지 마세요 |
| `server.go` | `Deps` / `New` / chi 라우터 / 미들웨어 / 요청 컨텍스트 |
| `actions.go` | `operationID → host.Action` 표와 public 라우트 집합 |
| `errors.go` | `apiError`와 sentinel → status/code 매핑 한 곳 |
| `convert.go` | store ↔ wire 변환, enum, 페이지네이션, Duration, 비밀 암호화 |
| `settings.go` | `/healthz`, 테넌트 설정 |
| `sending.go` | transport / sender / sending-domain / probe mailbox / probe run / health |
| `content.go` | layout / template / i18n / preview / publish / message version |
| `campaigns.go` | 캠페인 CRUD · 인제스트 · 수명주기 · 링크 리포트 · 수신거부 통지 |
| `deliveries.go` | 테넌트 전역 delivery 목록 · delivery · attempt · bounce · retry |
| `messages.go` | 트랜잭셔널 발송과 멱등 재생 |
| `lists.go` | suppression / bounce / event(outbox) |
| `tracking.go` | 공개 `/t/*` 라우트, IP 토큰버킷, 봇 판정, 1×1 gif |

## 미들웨어 순서

```
chi:    request ID (+ Accept, client IP, UA를 컨텍스트에)
     →  recover → 500 JSON
     →  body size limit (Limits.MaxBodyBytes, NDJSON 인제스트만 예외)
     →  [생성된 라우터: 경로/쿼리/헤더 바인딩 → 실패 시 400]
strict: gate(operationID)
          public 라우트면 통과
          Authenticate       → 실패 401
          TenantResolver     → 빈 테넌트/_system 이면 403
          Authorize(action)  → 실패 403
          Provider.ForTenant → 컨텍스트에 tenant Store
     →  핸들러
```

`gate`가 chi 미들웨어가 아니라 **strict 미들웨어**인 이유는 오퍼레이션 ID를 아는 지점이 거기뿐이기 때문입니다.
`opActions`에 없는 오퍼레이션은 500으로 **거부**합니다 — 기본 허용이면 아무도 인가하지 않은 라우트를 서비스하게 됩니다(§16).
`TestEveryOperationHasAnAction`이 스펙을 yaml.v3로 다시 읽어 표와 대조하므로, 스펙에 오퍼레이션을 추가하고 표를 잊으면 테스트가 깹니다.

NDJSON 인제스트(`POST /campaigns/{id}/recipients`)만 바디 크기 제한에서 빠집니다.
`MaxBodyBytes`(기본 2 MiB)는 템플릿 본문 기준이고, 청크는 권장 10k~100k줄이라 그 한도를 씌우면 엔드포인트가 성립하지 않습니다.
대신 줄당 `MaxRecipientLineBytes`, 캠페인당 `MaxRecipientsPerCampaign`이 걸립니다(ADR-0005).

## 재생성

```
make gen          # go tool oapi-codegen + gofmt. 두 번 돌려도 diff 없음
make gen-check    # CI: 스펙과 gen.go가 어긋나면 실패
```

생성기는 `go.mod`의 `tool` 디렉티브로 고정되어 있어 따로 설치할 것이 없습니다.
`internal/api/server.go`의 `//go:generate`도 같은 명령입니다.

## 테넌트와 공개 라우트

인증 라우트의 테넌트는 `TenantResolver`가 정하고, 핸들러는 컨텍스트의 `store.Store`만 봅니다 — `Provider`를 볼 수 없으므로 실수로 다른 테넌트에 닿을 수 없습니다(ADR-0006).

공개 `/t/*` 라우트는 인증이 없으므로 테넌트를 **토큰 안에서** 꺼냅니다(`tracking.TenantOf`).
이 값은 MAC이 검증되기 전까지는 공격자 입력이므로 **키를 고르는 용도로만** 쓰고, 검증이 통과하면 그 토큰을 서명한 테넌트가 맞다는 증거가 됩니다(테넌트 ID도 MAC에 포함).
토큰에 테넌트가 없었다면 검증이 모든 테넌트의 키를 훑어야 했습니다.

## 몇 가지 결정

- **비밀(secret)**: 쓰기 때 `SecretCipher`로 암호화하고 응답에는 절대 넣지 않습니다(`has_password` 등). 업데이트에서 필드를 **생략하면 저장값 유지** — GET 바디를 그대로 PUT해도 지워지지 않습니다.
- **낙관적 동시성**: `version`을 그대로 store에 넘기고 `ErrConflict` → 409 `version_conflict`.
- **캠페인의 `template_id`**: 캠페인 행에 그대로 저장하고 **start 시점에** 템플릿의 published 버전으로 해석합니다(`control.StartCampaign`, 예약 캠페인은 scheduler가 승격할 때). 따라서 생성은 미발행 템플릿도 받아들이고, 그때까지 발행되지 않았으면 start가 422로 실패합니다. `version_id`를 직접 주면 그 버전에 고정되고 start는 건드리지 않습니다.
- **트랜잭셔널 멱등성**: `RecipientChunkRepo`를 sentinel 캠페인 `_transactional` + `msg:{key}`로 재사용합니다. 청크에는 카운트만 있고 delivery ID가 없으므로, `Idempotency-Key`가 있으면 delivery ID를 `uuidv5(tenant, key, index, email)`로 **결정적으로** 만듭니다. 재생은 같은 ID를 다시 계산해 조회합니다.
- **필터가 있는 목록**: `BounceRepo.List`/`OutboxRepo.List`에 타입 필터가 없어 한 페이지를 가져와 걸러냅니다. 커서는 스토어 페이지 단위로 전진하므로 **필터된 페이지가 `limit`보다 적을 수 있습니다** — 클라이언트는 `next_cursor`가 없어질 때까지 따라가면 됩니다.
- **`GET /deliveries`**: 테넌트 전역 목록입니다(`DeliveryRepo.List`). 주소로 찾는 사람은 어느 캠페인인지 모르고, 트랜잭셔널·프로브 딜리버리에는 스코프로 삼을 캠페인 자체가 없습니다. 캠페인 스코프 라우트와 달리 존재 확인을 할 대상이 없으므로 **404를 내지 않습니다** — 없는 `campaign_id`는 빈 페이지입니다. "캠페인 없는 딜리버리"는 빈 `campaign_id`가 아니라 `lane`으로 고릅니다(빈 문자열이 "없음"을 뜻하는 쿼리는 URL에서 생략과 구분되지 않습니다). 필터는 `DeliveryFilter`에 그대로 실려 스토어가 키셋 인덱스 위에서 거릅니다.
- **`CampaignStats.sent` / `total`**: `by_status`에서 파생합니다(`statsTotals`). `sent`는 `sent + bounced + complained`입니다 — 바운스·불만은 MTA가 **받은 뒤** 며칠 지나 도착하므로, 그때 분모가 줄어들면 이미 발표한 오픈율이 저절로 올라갑니다. 비율의 분모를 클라이언트마다 다르게 고르는 일도 막습니다.
- **`GET /events/{id}`**: `OutboxRepo.Get(id)` 한 번입니다.
- **`POST /events/{id}/replay`**: `OutboxRepo.Reset(id, now)` — pending + `attempts = 0` + 에러/리스 초기화. 원인을 고치고 다시 보내는 것이므로 시도 예산을 온전히 돌려줍니다.
- **`POST /messages`의 `headers`**: `store.Delivery.Headers`에 저장하고 sender가 화이트리스트를 통과시켜 실제 메일에 붙입니다. 검증은 sender와 **같은** 화이트리스트(`sender.ValidateCustomHeader`)로 하므로 허용되지 않는 이름은 발송 시점이 아니라 여기서 422입니다.
- **`POST /senders/{id}/probe`**: `Deps.Probe`가 없으면(=`Options.Probe.Enabled`가 false면) 501. 프로브 구현은 이 패키지 밖입니다.
- **`/api/v1/bounce-mailboxes`**: 프로브 메일박스와 같은 모양의 CRUD. `after_process`는 `internal/mailbox.ParseAction`으로 검증하고, POP3에 `move:`는 422입니다 — 폴러가 매번 실패하는 행을 저장하지 않기 위해서입니다.
- **tracking 서명 키**: `SecretCipher`를 **타지 않습니다**(`store.SigningKey` 참조). 토큰을 검증하는 모든 경로가 키를 그대로 읽고, 그 복제본에 cipher가 없을 수도 있기 때문입니다. 생략하면 저장값 유지라는 규칙은 동일합니다.

## 테스트

```
go test -race ./internal/api/
```

`httptest` + `memstore` + 고정 시계 + 스텁 `Authenticator`/`Authorizer`(`harness_test.go`).
트래킹 테스트는 `TrackingBuffer.Close()`로 강제 flush한 뒤 delivery의 `first_*` 컬럼과 유니크 카운트를 확인합니다.
템플릿 픽스처는 MJML이 아니라 `mode: html`입니다 — MJML 컴파일은 publish당 수백 ms라서 MJML이 주제가 아닌 테스트에 넣을 비용이 아닙니다.
