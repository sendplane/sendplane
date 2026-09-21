# internal/probe

루프백 발신 헬스 체크. 실제 sender 경로로 프로브 메일을 보내고 프로브 메일박스에서 회수해
**수신 측 MTA가 붙인 헤더**로 판정합니다.
설계 근거는 [architecture.md §11](../../docs/architecture.md), [ADR-0012](../../docs/adr/0012-loopback-health-probe.md).

이 패키지는 **메일을 보내지 않습니다.** `lane=probe` Delivery를 큐에 넣으면 일반 sender가 실어 나릅니다 —
다른 경로로 보내면 검사할 가치가 있는 그 경로를 검사하지 못하기 때문입니다(ADR-0012).

## 흐름

```
Trigger(st, senderID)                 ← API POST /senders/{id}/probe, 또는 Tick
  메일박스마다 (kind 무관):
    Delivery{Lane: probe, SenderID, VersionID: 내장 프로브 버전,
             Email: mailbox.Address, Vars: {run_id, probe_token, mailbox}}
    ProbeRun{Pending: true, GroupID: 트리거당 하나, MailboxID, DeliveryID, StartedAt: now}
        ↓ (sender가 렌더·서명·발송. 제목은 "[sendplane probe {run_id}]")

kind=imap:  CollectWith(st, opener, now)        ← probe-collect 루프, 1분
  pending run마다:
    FetchByHeader("X-Sendplane-Probe", token) → 없으면 FetchByHeader("Subject", "[sendplane probe {run_id}]")
    찾음   → Evidence{Headers, Folder, ReceivedAt} → CompleteRun → 메일 삭제
    못 찾음 → StartedAt + Timeout(15m) 경과 시 미수신 처리 (연속 2회에서 red, ADR-0012)

kind=webhook: POST /probe/inbound/{provider}    ← 프로바이더가 호출 (ADR-0016)
    Verify(서명) → Parse → X-Sendplane-Probe 토큰 → 테넌트/run 해석 → 토큰 MAC 검증
    → Evidence{Headers: HeadersFromMap(...), Folder: ""} → CompleteRun → RefreshSenderHealth
  CollectWith 는 이 메일박스에 접속하지 않고 **타임아웃만** 적용합니다.

CompleteRun(st, run, Evidence, now)   ← 두 채널의 공통 경로
  헤더 파싱 → Verdict(§11.4) → DNS 진단(관측 IP) → ProbeRun 갱신
메일박스별 최신 run의 **최악 값** → Sender.Health/HealthReason/HealthCheckedAt (낙관적 갱신)
상태가 바뀌었을 때만 outbox에 `sender.health_changed`
```

`Tick(st, now)` 은 주기 경로입니다: `HealthCheckedAt` 이 없거나 `Interval`(기본 6h)보다 오래됐거나,
transport/도메인의 `UpdatedAt` 이 마지막 검사 이후로 움직인 sender를 트리거합니다.
타임아웃 안에 pending run이 있으면 트리거하지 않습니다 — 메일이 두 배가 되고 "연속 실패" 계산이 무의미해집니다.

## 수신 채널 (ADR-0016)

프로브 메일이 돌아오는 길은 둘이고, `store.ProbeMailbox.Kind` 가 고릅니다.

| kind | 회수 | 메일박스 행 | 폴더 |
|---|---|---|---|
| `imap`(기본, 빈 값 포함) | sendplane이 로그인해 검색 | 호스트·포트·인증·폴더 매핑·`authserv-id` | inbox / spam 구분 |
| `webhook` | 받은 쪽이 sendplane으로 POST | 주소와 `authserv-id` 만 (나머지는 API가 422) | `unknown` |

`webhook` 엔드포인트는 **프로세스 전역**입니다 — 웹훅 URL은 포워더 설정에 적히는 URL 하나이고,
테넌트마다 다른 URL을 주려면 테넌트마다 호스트네임이 필요합니다. 테넌트는
`X-Sendplane-Probe` 토큰(`<tenant>/<run>/<mac>`)에서 나오고, MAC 검증은 그 테넌트의 run을 읽은 **뒤에** 합니다.
트래킹 라우트와 같은 식으로 마운트됩니다: 인증 없음, IP당 레이트 리밋, 1 MiB 바디 상한.

```yaml
probe:
  webhooks:
    - provider: sendplane
      secrets:                                   # `secret:` 는 1개짜리 설탕
        - ${SENDPLANE_PROBE_WEBHOOK_SECRET}
      path: ""                                   # 기본 /probe/inbound/sendplane
      tolerance: 300s                            # 서명 타임스탬프 허용 오차(기본 5m)
```

### 기본 포맷 `sendplane`

`internal/probe/inbound/sendplanehook`. **JSON을 POST할 수 있는 것이면 무엇이든** 만들 수 있는 바디입니다 —
수신 서비스의 웹훅, Cloudflare Email Worker, MX 위의 스크립트.

```json
{
  "from": "news@example.com",
  "to": ["probe@example.net"],
  "headers": {
    "Authentication-Results": ["mx.example.net; spf=pass; dkim=pass header.d=example.com; dmarc=pass"],
    "Received": ["...최신...", "...오래된..."],
    "X-Sendplane-Probe": ["acme/01JB.../9f2c..."]
  },
  "text": "sendplane loopback health probe.\r\n"
}
```

- `from` 만 필수입니다. 없으면 `400` — 메일이 아닌 바디를 200으로 삼키면 포워더를 잘못 연결한 사람이
  아무 신호도 못 받습니다. `to` · `text` · `headers` 는 선택.
- 모르는 필드는 무시합니다. 헤더 키는 `textproto.CanonicalMIMEHeaderKey` 로 정규화하고,
  **같은 이름 안의 값 순서는 그대로 보존해야 합니다** — `Received` 체인은 아래에서 위로 읽고,
  "첫 `Authentication-Results`"가 신뢰 판단이기 때문입니다.
- 바디 상한 1 MiB.

> **포워더는 `Authentication-Results` · `Received` · `X-Sendplane-Probe` 를 반드시 보존해야 합니다.**
> 앞의 둘이 없으면 판정 근거가 0이라 영원히 yellow이고(ADR-0016이 어떤 서비스의 네이티브 포맷을 기각한 바로 그 이유),
> 셋째가 없으면 배달을 테넌트에 붙일 수 없어 `200 ignored` 입니다.

### 서명

`X-Sendplane-Signature: t=<unix seconds>,v1=<hex>[,v1=<hex>...]`,
`v1 = hex(HMAC-SHA256(secret, "<t>" + "." + <원본 바디 바이트>))` (`internal/probe/inbound/sigv1`).

- 검증은 **원본 바디**로 합니다. 다시 마샬링한 JSON은 서명된 바이트가 아닙니다.
- 어느 `v1` 이든 어느 설정된 시크릿이든 **하나만 맞으면** 통과(상수 시간 비교).
  보내는 쪽은 `v1=` 를 둘 실어서, 받는 쪽은 `secrets:` 를 둘 적어서 **무중단 로테이션**을 합니다.
- `|now - t| > tolerance`(기본 300s)면 리플레이로 `401`.
- 시크릿은 접두사(`whsec_...`)까지 **원문 그대로** 키입니다. 잘라내면 안 됩니다.

셸에서 만들어 보내기 (`make dev` 의 `deploy/dev/config.yaml` 시크릿 기준):

```sh
BODY='{"from":"a@example.com","to":["probe@example.com"],"headers":{}}'
T=$(date +%s)
SIG=$(printf '%s.%s' "$T" "$BODY" \
  | openssl dgst -sha256 -hmac whsec_dev-probe-webhook -hex | sed 's/^.* //')

curl -sS -i -X POST http://localhost:8080/probe/inbound/sendplane \
  -H "Content-Type: application/json" \
  -H "X-Sendplane-Signature: t=$T,v1=$SIG" \
  --data-binary "$BODY"
# 200 ignored   ← 서명은 맞고, X-Sendplane-Probe 가 없으니 우리 메일이 아님
```

`--data-binary` 여야 합니다. `-d` 는 개행을 지워 서명이 덮은 바이트를 바꿔 버립니다.

### 응답

| | 언제 |
|---|---|
| `200 accepted` | run을 완료했거나 이미 완료돼 있었음(재전송) |
| `200 ignored` | sendplane 프로브가 아님 · 토큰이 위조됨 · run이 없음. **4xx면 안 됩니다** — 발신 측이 영원히 재시도합니다 |
| `400` | 서명은 맞는데 바디가 그 포맷이 아님. 같은 바이트를 다시 보내도 소용없음 |
| `401` | 서명 없음/깨짐/모르는 시크릿/타임스탬프 만료 |
| `413` | 바디 1 MiB 초과 |
| `503` | 지금 기록할 수 없음(스토어 장애). **보관했다가 재시도하세요** (`Retry-After`) |

매핑은 핸들러 한 곳(`internal/api/probeinbound.go`)에 있고 인터페이스에는 없습니다.

### 포맷 추가하기

다른 서비스의 네이티브 포맷을 그대로 받고 싶다면 `inbound.Provider` 세 메서드와 `init()` 하나입니다.

```go
package examplehook

import (
    "encoding/json"
    "fmt"
    "net/http"
    "net/textproto"

    "github.com/sendplane/sendplane/internal/probe/inbound"
)

type Provider struct{}

func init() { inbound.Register(Provider{}) }

func (Provider) Name() string { return "example" }

func (Provider) Verify(r *http.Request, body []byte, secrets []string) error {
    // 원본 body 바이트로, 상수 시간 비교로, 설정된 시크릿을 전부 시도(로테이션).
    // 실패는 inbound.ErrBadSignature 를 감싸서 돌려줍니다.
    // 같은 t=/v1= 스킴이면 sigv1.Verify(...) 한 줄로 끝납니다.
    return checkHMAC(r.Header.Get("X-Example-Signature"), body, secrets)
}

func (Provider) Parse(body []byte) ([]inbound.Message, error) {
    var p struct {
        Sender  string              `json:"sender"`
        Headers map[string][]string `json:"headers"`
    }
    if err := json.Unmarshal(body, &p); err != nil {
        return nil, fmt.Errorf("%w: %w", inbound.ErrBadPayload, err)
    }
    h := make(map[string][]string, len(p.Headers))
    for name, vs := range p.Headers { // 같은 이름 안의 순서는 보존
        k := textproto.CanonicalMIMEHeaderKey(name)
        h[k] = append(h[k], vs...)
    }
    return []inbound.Message{{
        ProviderID: inbound.BodyID(body), // 프로바이더 delivery ID가 있으면 그것
        From:       p.Sender,
        Headers:    h,
    }}, nil
}
```

그리고 루트 [`probe.go`](../../probe.go) 의 side-effect import 목록에 한 줄 추가하면
`probe.webhooks: [{provider: example, secrets: [...]}]` 가 동작합니다. 등록되지 않은 이름은 기동 시점에
등록된 이름 목록과 함께 거부됩니다. 서명에 타임스탬프가 있으면 `inbound.ToleranceSetter` 도 구현하세요 —
그래야 `tolerance:` 설정이 닿습니다(구현하지 않으면 그 설정은 기동 오류가 됩니다).

### 웹훅 프로브 메일박스 만들기

```sh
curl -X POST .../api/v1/probe-mailboxes -d '{
  "name": "example-forward", "kind": "webhook",
  "address": "probe@example.net", "authserv_id": "mx.example.net", "enabled": true}'
```

`authserv_id` 는 그 메일을 받은 MX가 `Authentication-Results` 에 쓰는 값이어야 합니다 —
일치하지 않는 헤더는 신뢰하지 않으므로(ADR-0012) 판정이 "판단 근거 없음" yellow로 고정됩니다.
`host`/`port`/`username`/… 를 같이 보내면 `422` 입니다(ADR-0016).

### 멱등성

두 겹입니다. 진짜 계약은 `ProbeRun.Pending` 이고(스토어에 있습니다), 그 앞에 `Message.ProviderID` 의
작은 LRU가 있어 완료된 run의 재전송은 스토어를 읽지도 않습니다. `sendplane` 포맷에는 트랜잭션 ID가 없으므로
**원본 바디의 sha256**(`inbound.BodyID`)이 그 자리입니다 — 재전송은 같은 바이트이고, 다른 프로브 메일은
최소한 토큰이 다릅니다. 서명은 타임스탬프 때문에 재전송마다 달라지므로 키가 될 수 없습니다.
완료된 run에 대한 재전송은 `200 accepted` 이고 아무것도 바뀌지 않습니다.

### 웹훅 메일박스의 헬스

로그인이 없으므로 `mailbox-check` 루프(루트 `mailboxcheck.go`)는 이 kind를 건너뜁니다. 대신:

- 프로브가 도착할 때마다 inbound 핸들러가 `ok` 를 씁니다.
- `probe-collect` 가 타임아웃으로 run을 닫으면 stage `webhook`,
  reason `no probe received within timeout` 으로 `error` 를 씁니다.

포워딩이 꺼져 버린 경우를 볼 수 있는 유일한 신호가 이것입니다.
`POST /probe-mailboxes/{id}/test` 는 이 kind에서 아무것도 다이얼하지 않고,
"이 배포에 포워더가 POST할 엔드포인트가 있는가"만 답합니다(`server: webhook:<provider>`).

## 판정 (§11.4)

| | 조건 |
|---|---|
| red | 미수신(연속 `ConsecutiveFailuresForRed`회, 기본 2) · `spf`/`dkim`/`dmarc` = fail |
| yellow | 스팸함 · `p=none` · TLS 없음 · PTR 불일치 · softfail/neutral/none · **신뢰 가능한 `Authentication-Results` 없음** |
| green | 전부 pass + (받은편지함 또는 폴더 미상) + TLS |

폴더 `unknown`(웹훅 채널)은 **판정을 내리지 않습니다.** yellow가 되는 폴더는
`FolderOther` — 메일박스가 폴더 매핑을 설정해 뒀는데 거기로 안 들어온 경우 — 뿐입니다.
모르는 것을 "스팸함"으로 읽으면 멀쩡한 sender가 영원히 yellow가 됩니다(ADR-0016).

여러 메일박스는 최악 값이 요약이고 상세는 메일박스별 `ProbeRun` 에 남습니다.
DNS 정적 검사는 **판정을 뒤집지 않습니다.** 루프백이 green인데 DNS가 red면 yellow로만 내립니다(ADR-0012: DNS는 진단 계층).

## 헤더 파싱 (`headers.go`)

- `Authentication-Results`(RFC 8601): authserv-id → `method=result` 절 → `header.d` / `header.s` / `smtp.mailfrom` 프로퍼티.
  주석(괄호)과 따옴표를 존중해 `;` 로 자릅니다 — Gmail의 SPF 주석에는 `;` 가 들어 있습니다.
  DMARC의 `p=` 는 대개 주석(`dmarc=pass (p=REJECT sp=REJECT dis=NONE)`)에만 있어 주석을 보존합니다.
- **authserv-id가 메일박스 설정값과 일치하는 헤더만 신뢰합니다**(ADR-0012). 상류의 누구나 이 헤더를 붙일 수 있습니다.
  예외: **Exchange Online은 authserv-id를 생략**합니다. 해당 메일박스는 `ProbeMailbox.AuthServID` 를 비워 두고,
  대신 프로브 토큰(HMAC)이 메일의 진위를 보증합니다.
- `Received` 체인: 헤더는 위가 최신이므로 **아래에서 위로** 읽어 공인 IP가 처음 기록된 홉이 조직 경계입니다.
  `(rdns [ip])`(Postfix/Gmail/Sendmail)와 `(ip)`(Exchange) 두 형태를 모두 읽고, `with ESMTPS`·`version=TLS`·`cipher=` 로 TLS를 판정합니다.
- 지연: `Date`(없으면 가장 오래된 `Received`) → 메일박스 수신 시각. 조직 간 시계 오차로 음수가 나오면 0입니다.

픽스처는 `testdata/` 에 Gmail(pass / dkim fail+스팸), Outlook(Exchange Online), Postfix+OpenDMARC 4종입니다.

## 내장 프로브 메시지

`Template` 이 아니라 **`MessageVersion` 하나**를 테넌트별로 만들어 씁니다.

- 프로브 본문은 편집 대상이 아니고, `MessageVersion` 은 불변이라 고정 메시지에 맞습니다. 운영자의 템플릿 목록도 더럽히지 않습니다.
- 고정 UUID를 쓰지 않은 이유: `message_version.id` 가 **전역 PK**라 테넌트 둘이 같은 ID를 가질 수 없습니다.
  대신 `TemplateID = "_sendplane_probe"`(마커) + `Checksum = "sendplane-probe-v1"` 로 찾고, 없으면 만듭니다.
  템플릿을 바꾸려면 체크섬을 올리면 새 버전이 생기고, 진행 중인 프로브는 자기 버전을 그대로 씁니다(§6.3).
- 제목 `[sendplane probe {{ vars.run_id }}]`, 본문에 run id. `Delivery.Vars` 로 바인딩됩니다.

## 루트/control 배선

```go
runner := probe.New(probe.Options{
    Interval: 6 * time.Hour,
    Timeout:  15 * time.Minute,
    HMACKey:  probeKey,                       // 없으면 토큰 = "<tenant>/<run>" (MAC 없음)
    DNS:      dnscheck.New(resolver),         // nil이면 진단 계층 생략
    Secrets:  opts.Secrets,                   // DKIM 개인키 복호화(공개키 비교용)
})
```

1. **control 리더 루프**에 두 개를 등록합니다(둘 다 리더 1대에서만 돌아야 합니다 — 중복 발송/중복 회수 방지).
   `internal/control` 의 `tickLoop` 은 `Tick(ctx, now) error` 이므로 얇은 어댑터가 필요합니다:

   ```go
   type probeLoop struct{ r *probe.Runner; st store.Store }
   func (l probeLoop) Tick(ctx context.Context, now time.Time) error { return l.r.Tick(ctx, l.st, now) }

   control.WithLoop(control.Loop{
       Name: "probe-collect", Interval: time.Minute, AllTenants: true,
       NewTenant: func(st store.Store, _ string) control.TickLoop { return probeLoop{r: runner, st: st} },
   })
   ```

   | 루프 | 주기(제안) | 하는 일 |
   |---|---|---|
   | `probeTrigger` | 5m | `Runner.Tick` — 만기 sender 트리거 |
   | `probeCollect` | 1m | `Runner.CollectWith` — 메일박스 회수·판정 |

   둘 다 **`control.Loop.AllTenants`** 로 등록해야 합니다. 프로브 delivery는 1~2초면 `sent`(종단)가 되어
   테넌트가 곧바로 `Provider.ActiveTenants`에서 빠지므로, active 테넌트만 도는 회수 루프는 **테넌트가 마침
   다른 일을 하고 있을 때만** 판정을 끝냅니다. `internal/bounce`가 `Provider.Tenants`를 쓰는 것과 같은 이유입니다.

2. **메일박스 어댑터**: 루트의 [`probe.go`](../../probe.go) 가 `internal/mailbox` 의 `Client` 를
   `probe.MailboxOpener` 로 감쌉니다(`probeOpener`/`probeFetcher`). `probe` 는 `internal/mailbox` 를
   import하지 않습니다 — 필요한 건 두 메서드뿐이고, 두 패키지를 다 아는 곳은 루트입니다.

   - `FetchByHeader` → `[]probe.RawMessage{ID, Folder, Raw, ReceivedAt}`, `Delete` → `Ack(..., ActionDelete)`.
   - IMAP 커넥션은 폴더 하나를 SELECT하므로 **받은편지함과 스팸함에 각각 커넥션을 엽니다.**
     받은편지함만 보면 스팸함에 들어간 메일이 "미수신"으로 보입니다 — 그게 yellow 판정이 존재하는 이유입니다.
     UID는 폴더 안에서만 유일하므로 ID에 폴더를 붙여(`folder\x00uid`) `Delete` 가 올바른 커넥션으로 보냅니다.

   `Open` 이 돌려준 fetcher가 `io.Closer` 면 그 메일박스를 끝낸 뒤 닫습니다.
   단일 fetcher만 있는 경우를 위해 `Collect(ctx, st, fetcher, now)` 도 있습니다(테스트·단일 메일박스용).

3. **sender**: `lane=probe` 워커 수가 기본 0이므로 **프로브를 쓰려면 sender 설정에서 probe 레인 워커를 ≥1로 올려야 합니다.**
   0이면 Delivery가 영원히 queued로 남고 모든 run이 15분 뒤 미수신으로 닫힙니다.

4. **API**: `POST /senders/{id}/probe` → `Trigger` → 반환된 run id. 프로브 메일박스가 하나도 없으면
   `ErrNoMailbox`(DNS 체커가 있으면 DNS 전용 run으로 대체하고 "loopback 미구성"을 상태 사유에 남깁니다, ADR-0012).

## sender 쪽

`renderMessage` 가 `Delivery.Vars["probe_token"]` 을 `X-Sendplane-Probe` 헤더로 내보내므로
`Collect` 의 헤더 검색이 1순위로 동작하고 제목 폴백은 호환 경로로만 남습니다.
`X-` 접두사는 헤더 화이트리스트를 통과합니다(`internal/sender/message.go`).

`lane=probe` 는 **트래킹(오픈 픽셀·링크 재작성·수신거부)과 suppression에서 모두 제외**됩니다(§11.2, ADR-0012).
프로브 메일은 sendplane이 소유한 메일박스로 가므로 오픈/클릭 이벤트를 남길 이유가 없습니다.

## 메일박스 헬스

수집 루프는 메일박스를 열 때마다 그 결과를 `ProbeMailbox.Health`(architecture 11.5)에 기록합니다
(`internal/mbhealth`). 열지 **못했는데** 타임아웃까지 지난 run은 red "미수신"이 아니라

```
Status = unknown
Reason = "probe mailbox unreachable: auth"   // 또는 dial / tls / folder
```

으로 닫힙니다. 아무것도 관측하지 못한 실행을 sender 탓으로 돌리면 로테이션된 IMAP 비밀번호가
"이 sender는 red"로 나타나고, 그러면 DNS 를 뒤지게 됩니다(ADR-0015). `undeliveredStreak` 도 그런 run을
건너뜁니다 — 증거가 없는 실행은 연속 실패를 늘리지도 끊지도 않습니다.

단계는 opener 가 돌려주는 에러에서 읽습니다. 이 패키지는 `internal/mailbox` 를 import 하지 않으므로
한-메서드 인터페이스(`interface{ MailboxStage() string }`)로만 봅니다. 루트의 어댑터가 감싸는
`mailbox.DialError` 가 그걸 구현합니다.

수집 루프는 **pending run 이 있을 때만** 메일박스를 엽니다 — 6시간 주기라면 6시간에 몇 분입니다.
그 사이의 공백은 control 리더의 `mailbox-check` 루프(루트 `mailboxcheck.go`, 기본 15분)가 메웁니다.

## 스토어 계약

`ProbeRunRepo` 는 `Create`/`Update`/`Get`/`ListBySender`/`ListPending` 입니다. run은 `Pending: true` 로
쓰이고 메일이 도착하거나 타임아웃이 지날 때 한 번 갱신됩니다(타입 단언 없이 `Update` 를 그대로 부릅니다).
`Collect` 과 `Tick` 은 `ListPending` 을 한 번 읽습니다 — sender마다 전체 이력을 페이징하지 않습니다.
한 번의 `Trigger` 가 만든 run들은 `GroupID` 를 공유하고 API의 `ProbeRun` 스키마에 `group_id`/`pending` 으로 나옵니다.

남은 갭:

1. **`SendingDomain.OutboundIPs` 를 갱신하지 않습니다.** 관측 IP는 `ProbeRun.ObservedIP` 에만 남깁니다.
   도메인 행까지 쓰려면 낙관적 갱신 한 번이 더 필요하고, 그 소유권은 control에 두는 편이 맞아 보여 남겨 뒀습니다.
2. **`Trigger` 는 여전히 첫 메일박스의 run ID를 돌려줍니다.** `GroupID` 로 묶어 한 번에 조회하려면
   `ProbeRunRepo.ListByGroup` 이 있어야 합니다(지금은 API가 그 한 run만 보고합니다).

## 테스트

```
go test -race ./internal/probe/...
```

`memstore` 기반으로 트리거·회수·타임아웃·최악 값·DNS 연동·이벤트·주기를 모두 덮습니다.
헤더 파서는 `testdata/` 픽스처로 따로 돕니다.
`inbound/sigv1` 은 서명 스킴 자체를(유효·잘못된 시크릿·만료·미래 타임스탬프·`v1` 여러 개·
로테이션된 시크릿·깨진 헤더), `inbound/sendplanehook` 은 그 위에서 파싱을(헤더 정규화·순서 보존·
`from` 누락 400·1 MiB 초과·모르는 필드 무시) 덮습니다.
HTTP 핸들러 쪽은 `internal/api/probeinbound_test.go` 에 있습니다
(정상 완료·중복 전송·잘못된 서명·만료된 서명·로테이션·프로브 아닌 메일·깨진 바디 400·503 후 재시도·미설정 404).
e2e는 `test/e2e/scenario_probe_webhook.go` 가 GreenMail에 도착한 실제 프로브 메일을
`sendplane` 바디로 바꿔 `sigv1.Sign` 으로 서명해 보냅니다.
