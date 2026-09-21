# internal/bounce

IMAP/POP3 메일박스의 메일을 delivery 상태로 바꾸는 경로입니다: **파싱 → 상관관계 → BounceEvent →
delivery 전이 → suppression → 호스트 이벤트**.
설계 근거는 [architecture.md §4.1, §10, §12](../../docs/architecture.md),
[ADR-0008](../../docs/adr/0008-bounce-and-suppression.md).

## 세 층

| 파일 | 하는 일 | 상태 |
|---|---|---|
| `parse.go`·`classify.go` | raw 바이트 → `Parsed`. 순수 함수 | 없음 |
| `process.go` | `Parsed` → 한 테넌트의 스토어 쓰기 | 없음(테넌트별 인스턴스 불필요) |
| `poller.go` | 메일박스별 폴링 + 스토어 lock | 메일박스별 고루틴 |

## 파싱과 분류

```go
func Parse(raw []byte) (Parsed, error)                                  // 키 없이 (VERP는 Verified=false)
func ParseWithKeys(raw []byte, keys []store.SigningKey) (Parsed, error) // 프로세서가 쓰는 쪽
```

- **구조화된 리포트 우선**: `message/delivery-status`(RFC 3464) → Action/Status/Diagnostic-Code,
  `message/feedback-report`(RFC 5965) → complaint. 둘 다 "빈 줄로 나뉜 RFC 822 헤더 그룹"이라 직접 읽습니다.
  MIME 워크는 깊이 8, 파트당 1MiB로 제한합니다(§16 — 바운스 메일박스는 외부 입력입니다).
- **리포트 파트가 없을 때만 휴리스틱**: 제목/본문 패턴 + 본문에서 찾은 enhanced status. `classify.go`의 표는
  일부러 짧습니다 — 프로바이더마다 규칙을 늘리는 대신 흔한 모양만 맞히고 나머지는 `low` confidence로 둡니다.
- **자동응답**은 `Auto-Submitted`, `X-Autoreply`, 제목 패턴(영어·한국어)으로 걸러 `AutoReply=true`로 표시하고 무시합니다.
  단 `Auto-Submitted: auto-replied`는 **DSN도 달고 나오므로**(RFC 3834) 리포트 파트가 없는 메일에만 적용합니다.

| 판정 | 규칙 |
|---|---|
| `hard` | `Action: failed` + 5.x.x (아래 soft 예외 제외) |
| `soft` | 4.x.x, `Action: delayed`, 그리고 5.x.x 중 **주소가 아니라 메시지·수신 시스템·우리 쪽 문제**인 것: `5.2.2`(mailbox full), `5.2.3`, `5.3.*`, `5.4.*`, `5.5.*`, `5.7.*`(정책/평판) |
| `complaint` | `Feedback-Type: abuse|fraud` |
| `unknown` | 성공 DSN(`delivered/relayed/expanded`), `not-spam`, 무관한 메일 |

`5.7.*`를 hard로 보지 않는 이유: 한 수신 서버가 정책으로 한 통을 막은 것 때문에 주소를 영구 차단하면,
다시 보내는 것보다 피해가 큽니다. 기록은 남고 호스트는 이벤트로 봅니다.

## 상관관계 (§10의 3중화)

순서는 **VERP → `X-Sendplane-ID` → `Message-ID`** 입니다.

1. `Return-Path`/`To`/`Delivered-To`/`X-Original-To`/`Envelope-To`, DSN의 `X-Postfix-Sender`·`Final-Recipient`(이중 바운스),
   ARF의 `Original-Mail-From`, 그리고 **반송된 원본의 `Return-Path`** 에서 VERP를 찾아
   `internal/tracking.ParseVERP`로 HMAC 검증.
2. 반송된 원본(또는 최상위)의 `X-Sendplane-ID: {tenant}/{deliveryID}`.
3. 반송된 원본의 `Message-ID: <{deliveryID}@{domain}>`, 최상위 `In-Reply-To`/`References`. 로컬파트가
   UUID 모양일 때만 받아들입니다.

**VERP가 있는데 MAC이 틀리면 헤더로 다시 시도하지 않고** `Verified=false`로 끝냅니다. 그러지 않으면
공격자가 자기가 받은 메일에서 `X-Sendplane-ID`를 베껴 위조 바운스를 세탁할 수 있습니다.
반대로 VERP가 아예 없어 헤더/Message-ID로 찾은 경우는 `Verified=true`입니다 — MAC은 아니지만 반박된 것도 없고,
delivery가 그 테넌트에 실제로 있고 `sent`여야 한다는 조건이 남아 있습니다(릴레이가 envelope sender를 덮어쓰는 환경에서
헤더 경로만 남는다는 ADR-0008의 전제).

## Processor

```go
func NewProcessor(Options) *Processor
func (p *Processor) Handle(ctx, st store.Store, tenantID string, msg mailbox.Message) (Outcome, error)
```

`st`는 `tenantID`의 스토어여야 합니다. `store.Store`에서 테넌트를 역으로 알아낼 수 없는데,
`X-Sendplane-ID`가 **다른 테넌트**를 가리키는 경우를 거르려면 테넌트 ID가 필요합니다.

**error를 반환하면 스토어가 실패한 것**이고, 호출자는 그 메일을 ack하면 안 됩니다.
파싱 불가·모르는 delivery·위조 MAC은 error가 아니라 `Outcome.Skipped`입니다 — 재시도해 봐야 영원히 같은 메일만 다시 받습니다.

| 상황 | BounceEvent | delivery | suppression | outbox |
|---|---|---|---|---|
| hard, `sent` | ✓ | → `bounced` | ✓(설정 on, `lane != probe`) | `delivery.bounced` |
| complaint, `sent` | ✓ | → `complained` | ✓ | `delivery.complained` |
| soft, `sent` | ✓ | 유지 | ✗ | ✗ |
| soft, 이미 `bounced` | ✓ | 유지 | ✗ | ✗ |
| hard 재전달(이미 `bounced`) | ✗ | 유지 | ✗ | ✗ |
| `sent`가 아닌 delivery | ✗ | 유지 | ✗ | ✗ |
| VERP 위조(`Verified=false`) | ✓ (`Verified=false`) | 유지 | ✗ | ✗ |
| 상관관계 실패 / 모르는 delivery | ✓ (`DeliveryID` 빈 값 또는 그대로) | — | ✗ | ✗ |
| 자동응답 / 바운스 아님 / 다른 테넌트 | ✗ | — | ✗ | ✗ |

멱등성은 `DeliveryRepo.MarkBounced`의 CAS(`status = sent`)가 만듭니다. 같은 DSN이 두 번 오면 두 번째는
`changed=false`를 받고 이벤트도 남기지 않습니다. 프로브 lane은 suppression에서 제외합니다(ADR-0012).

## Runner (폴러)

```go
func NewRunner(RunnerConfig) (*Runner, error)
func (r *Runner) Run(ctx context.Context) error                     // ctx까지 블록
func (r *Runner) PollOnce(ctx, box TenantMailbox) (Stats, error)    // 한 패스(테스트/수동 트리거용)
func LockName(mailboxID string) string                              // "bounce:" + mailboxID
```

- `MailboxSource.ListMailboxes(ctx)`를 `RefreshInterval`(기본 5분)마다 다시 읽어 **메일박스당 고루틴 하나**를 띄웁니다.
  `Provider.ActiveTenants`는 쓰지 않습니다 — 바운스는 캠페인이 끝나고 한참 뒤에 도착하고, 그때 테넌트는 이미 active가 아닙니다.
- 한 패스: 그 테넌트 스토어에서 `Locks().Acquire("bounce:"+mailboxID, owner, LockTTL, now)` →
  dial → 50개씩 `Fetch` → `Handle` → 정책대로 `Ack` → **배치마다 lock 갱신** → 배치가 덜 찼으면 종료 → `Release`.
  lock을 못 잡으면 `Stats.Locked=false`로 즉시 반환합니다(리더 한 대 빼고는 그게 정상 상태).
- 갱신에 실패하면(= 다른 복제본이 가져감) **그 자리에서 쓰기를 멈춥니다.**
- `UseIdle`이 켜져 있고 서버가 IDLE을 지원하면 패스 끝에서 IDLE로 대기하고, 깨면 바로 다음 패스로 갑니다.
- 실패는 메일박스별 지수 백오프(`PollInterval` → `MaxBackoff`)이고 **Runner는 절대 죽지 않습니다.**
  망가진 IMAP 계정 하나가 프로세스를 내리면 안 됩니다.

## 루트 패키지 연결 (`RunBounce`)

이 패키지는 루트를 import하지 않습니다. 루트의 [`bounce.go`](../../bounce.go)가 배선합니다.

- **메일박스 목록**: `bounceMailboxes`가 `store.Provider.Tenants`(= 설정 행이나 설정성 행이 있는 **모든** 테넌트)를 돌며
  각 테넌트의 `BounceMailboxes().ListEnabled`를 읽어 `bounce.TenantMailbox`로 변환합니다.
  `ActiveTenants`를 쓰지 않는 이유가 그대로 여기 있습니다 — 바운스는 캠페인이 끝나고 한참 뒤에 도착합니다.
  `AfterProcess`가 파싱되지 않는 행은 로그만 남기고 건너뜁니다(한 행 때문에 전체 새로고침이 멈추면 안 됩니다).
- **역할 진입점**: `(*Sendplane).RunBounce(ctx, BounceConfig)`. `BounceConfig`는 lock owner(`WorkerID`,
  비우면 호스트명+PID로 생성), `PollInterval`, `RefreshInterval`, `UseIdle`입니다.
  `cmd/sendplane`의 `bounce:` 섹션이 그대로 매핑됩니다.

`Processor`/`Runner` 둘 다 `host.SecretCipher`·`host.Metrics`를 leaf 패키지 `host`에서 받으므로
루트가 별칭으로 재노출한 타입을 그대로 넘길 수 있습니다(§2.1, internal/sender와 같은 구조).

## store 계약

메일박스는 이제 테넌트 리소스입니다: `store.BounceMailbox` + `BounceMailboxRepo`
(CRUD + `ListEnabled`), API는 `/api/v1/bounce-mailboxes`. `Protocol`·`Folder`·`AfterProcess`·`Enabled`를
행이 들고 있고 비밀번호는 `SecretCipher` 암호문입니다.
raw 보존은 `TenantSettings.BounceRetainRaw`(기본 off)이고 프로세서는 테넌트 설정을 읽습니다 — §10 그대로입니다.
suppression 보존기간은 `SuppressionRepo.DeleteBefore`(만료가 **있는** 항목만)로 control의 retention 루프가 정리합니다.

## 메일박스 헬스

패스마다 다이얼 결과를 `BounceMailbox.Health`(architecture 11.5)에 씁니다 — 성공은 `ok`, 실패는
`mailbox.StageOf(err)` 가 준 단계(`dial`/`tls`/`auth`/`folder`)와 사유, 그리고 연속 실패 카운트입니다
(`internal/mbhealth`). 연속 2회에서 `mailbox.unhealthy`, 복구되면 `mailbox.recovered` 이벤트가 한 번 납니다.

바운스 메일박스를 주기적으로 들여다보는 건 이 폴러뿐이라 여기서 안 보면 아무도 못 봅니다. 폴링을
끈 배포를 위해서는 control 리더의 `mailbox-check` 루프가 같은 일을 합니다.

헬스는 `TenantMailbox` 가 아니라 **스토어 행에서 다시 읽습니다**: 메일박스 목록은 `RefreshInterval`
(기본 5분)마다 갱신되므로 목록에 실린 헬스는 그만큼 낡았고, 연속 실패는 현재 값에서 세지 않으면
두 레플리카가 서로의 카운트를 되돌립니다. 헬스 쓰기 실패는 로그만 남기고 패스를 실패시키지 않습니다 —
메일을 빼내는 게 본업입니다.

아직 계약에 없는 것:

1. **`store.BounceType`에 auto-reply 값이 없습니다.** 자동응답은 저장하지 않으므로 `Parsed.AutoReply` 플래그로만 둡니다.
2. **`store.BounceSource`에 "매칭 실패" 값이 없습니다.** 상관관계가 아예 안 된 이벤트는 `heuristic`으로 기록합니다.

### store에 추가한 것

`DeliveryRepo.MarkBounced` / `MarkComplained` (`(bool, error)`). DSN은 lease가 없는 비동기 전이라
기존 `Complete`(lease CAS)로는 표현할 수 없었습니다. CAS는 `status = sent` 하나이고,
`MarkSent`가 남겨 둔 lease를 지웁니다 — 결과 flush 창 안에 DSN이 들어와도 `Complete`가 bounced를 sent로
되돌리지 못하게 하기 위해서입니다(대신 attempt 이력 한 줄을 잃을 수 있고, 그게 더 싼 쪽입니다).
계약·`storetest`·memstore·postgres·mongo 모두 반영했고 스키마 변경은 없습니다.

## 테스트

```
go test -race ./internal/bounce/...
```

- `testdata/`의 실제 모양 픽스처 11종(Postfix·Exim·Gmail DSN, Outlook NDR, ARF, soft 4.2.2, delayed,
  자동응답 2종, 무관한 메일, 위조 VERP)을 테이블로 돌립니다. 픽스처의 VERP 주소는
  `tracking.VERPAddress`로 만든 진짜 값이고 `TestFixtureVERPAddresses`가 그걸 고정합니다.
- 프로세서는 memstore + 고정 시계, 폴러는 `mailbox.Fake`를 씁니다.
