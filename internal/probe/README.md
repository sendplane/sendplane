# internal/probe

루프백 발신 헬스 체크. 실제 sender 경로로 프로브 메일을 보내고 프로브 메일박스에서 회수해
**수신 측 MTA가 붙인 헤더**로 판정합니다.
설계 근거는 [architecture.md §11](../../docs/architecture.md), [ADR-0012](../../docs/adr/0012-loopback-health-probe.md).

이 패키지는 **메일을 보내지 않습니다.** `lane=probe` Delivery를 큐에 넣으면 일반 sender가 실어 나릅니다 —
다른 경로로 보내면 검사할 가치가 있는 그 경로를 검사하지 못하기 때문입니다(ADR-0012).

## 흐름

```
Trigger(st, senderID)                 ← API POST /senders/{id}/probe, 또는 Tick
  메일박스마다:
    Delivery{Lane: probe, SenderID, VersionID: 내장 프로브 버전,
             Email: mailbox.Address, Vars: {run_id, probe_token, mailbox}}
    ProbeRun{Pending: true, GroupID: 트리거당 하나, MailboxID, DeliveryID, StartedAt: now}
        ↓ (sender가 렌더·서명·발송. 제목은 "[sendplane probe {run_id}]")
Collect(st, fetcher, now)             ← 주기 호출
  pending run마다:
    FetchByHeader("X-Sendplane-Probe", token) → 없으면 FetchByHeader("Subject", "[sendplane probe {run_id}]")
    찾음   → 헤더 파싱 → Verdict(§11.4) → DNS 진단(관측 IP) → ProbeRun 갱신 → 메일 삭제
    못 찾음 → StartedAt + Timeout(15m) 경과 시 미수신 처리 (연속 2회에서 red, ADR-0012)
  메일박스별 최신 run의 **최악 값** → Sender.Health/HealthReason/HealthCheckedAt (낙관적 갱신)
  상태가 바뀌었을 때만 outbox에 `sender.health_changed`
```

`Tick(st, now)` 은 주기 경로입니다: `HealthCheckedAt` 이 없거나 `Interval`(기본 6h)보다 오래됐거나,
transport/도메인의 `UpdatedAt` 이 마지막 검사 이후로 움직인 sender를 트리거합니다.
타임아웃 안에 pending run이 있으면 트리거하지 않습니다 — 메일이 두 배가 되고 "연속 실패" 계산이 무의미해집니다.

## 판정 (§11.4)

| | 조건 |
|---|---|
| red | 미수신(연속 `ConsecutiveFailuresForRed`회, 기본 2) · `spf`/`dkim`/`dmarc` = fail |
| yellow | 스팸함 · `p=none` · TLS 없음 · PTR 불일치 · softfail/neutral/none · **신뢰 가능한 `Authentication-Results` 없음** |
| green | 전부 pass + 받은편지함 + TLS |

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
    HMACKey:  probeKey,                       // 없으면 토큰 = run id
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
