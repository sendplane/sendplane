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
    ProbeRun{Status: unknown(=pending), MailboxID, DeliveryID, StartedAt: now}
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
   ```

   | 루프 | 주기(제안) | 하는 일 |
   |---|---|---|
   | `probeTrigger` | 5m | `Runner.Tick` — 만기 sender 트리거 |
   | `probeCollect` | 1m | `Runner.CollectWith` — 메일박스 회수·판정 |

2. **메일박스 어댑터**: 루트가 `internal/mailbox` 의 `Client`(`Fetch`/`FetchByHeader`/`Ack`/`Close`)를
   `probe.MailboxOpener` 로 감쌉니다. `probe` 는 `internal/mailbox` 를 import하지 않습니다(이 패키지가 먼저 필요했고,
   좁은 인터페이스면 충분합니다).

   ```go
   type mailboxOpener struct{ /* SecretCipher 등 */ }
   func (o mailboxOpener) Open(ctx context.Context, m *store.ProbeMailbox) (probe.MailboxFetcher, error)
   // FetchByHeader → []probe.RawMessage{ID, Folder, Raw, ReceivedAt}, Delete → Ack/EXPUNGE
   ```

   `Open` 이 돌려준 fetcher가 `io.Closer` 면 그 메일박스를 끝낸 뒤 닫습니다.
   단일 fetcher만 있는 경우를 위해 `Collect(ctx, st, fetcher, now)` 도 있습니다(테스트·단일 메일박스용).

3. **sender**: `lane=probe` 워커 수가 기본 0이므로 **프로브를 쓰려면 sender 설정에서 probe 레인 워커를 ≥1로 올려야 합니다.**
   0이면 Delivery가 영원히 queued로 남고 모든 run이 15분 뒤 미수신으로 닫힙니다.

4. **API**: `POST /senders/{id}/probe` → `Trigger` → 반환된 run id. 프로브 메일박스가 하나도 없으면
   `ErrNoMailbox`(DNS 체커가 있으면 DNS 전용 run으로 대체하고 "loopback 미구성"을 상태 사유에 남깁니다, ADR-0012).

## sender에 필요한 한 줄

§11.2는 프로브 메일에 `X-Sendplane-Probe: {run_id}/{hmac}` 가 붙기를 요구하지만,
현재 sender는 **커스텀 헤더를 `Hooks.BeforeSend` 로만** 받습니다. `Delivery` 에는 `Headers` 필드가 없고,
`Delivery.Vars` 도 헤더로 옮겨지지 않습니다(`internal/sender/process.go` 의 `Headers: map[string]string{}`).
프로브 패키지가 훅에 의존할 수는 없으므로 **지금은 제목의 run id가 검색 키**입니다(`Collect` 이 헤더 → 제목 순으로 찾습니다).

sender 쪽 변경은 `internal/sender/process.go` 의 `renderMessage` 마지막 리턴 한 줄입니다:

```go
-		Headers:        map[string]string{},
+		Headers:        probeHeaders(d),   // d.Vars["probe_token"] 이 있으면 {"X-Sendplane-Probe": tok}
```

`X-` 접두사는 이미 헤더 화이트리스트를 통과하므로(`internal/sender/message.go`) 추가 허용은 필요 없습니다.
그때가 되면 `Collect` 의 헤더 검색이 그대로 1순위로 동작하고 제목 폴백은 호환 경로로 남습니다.

## 스토어 갭 (이 패키지 범위 밖, 변경 제안)

1. **`store.ProbeRunRepo` 에 `Update` 가 없습니다.** pending → finished 전이가 필요합니다.
   `memstore`/`postgres`/`mongo` 세 구현 모두 공용 CRUD에서 `Update` 를 이미 갖고 있어 런타임 타입 단언으로 동작하지만,
   인터페이스에 한 줄 추가하는 것이 옳습니다. 없으면 `Collect` 이 `ErrRunsImmutable` 을 돌려줍니다.
   ```go
   type ProbeRunRepo interface {
       Create(ctx context.Context, r *ProbeRun) error
       Update(ctx context.Context, r *ProbeRun) error   // ← 추가
       Get(ctx context.Context, id string) (*ProbeRun, error)
       ListBySender(ctx context.Context, senderID string, p Page) (Result[ProbeRun], error)
   }
   ```
2. **pending 상태 필드가 없습니다.** `Status == unknown && ReceivedAt.IsZero()` 를 pending으로 씁니다.
   완료된 run은 항상 green/yellow/red 중 하나라는 불변식에 기대는 것이라, `ProbeRun.Pending bool` 이나
   전용 열거형이 있으면 더 낫습니다.
3. **run 묶음 ID가 없습니다.** 한 번의 `Trigger` 는 메일박스 수만큼 run을 만드는데 이를 묶는 필드가 없어
   `StartedAt` 이 같은 것으로 묶습니다. `ProbeRun.GroupID string` 이면 API가 "이 트리거의 결과"를 그대로 조회할 수 있습니다.
   (현재 `Trigger` 는 첫 메일박스의 run ID를 돌려줍니다.)
4. **pending run 조회가 `ListBySender` 밖에 없습니다.** `Collect` 이 sender마다 전체 이력을 페이징합니다.
   보존기간이 이력을 잘라 주기 전까지는 비용이 이력 길이에 비례합니다. `ListPending(ctx, p)` 가 있으면 좋습니다.
5. **`lane=probe` 가 트래킹에서 제외되지 않습니다.** §11.2는 "통계·트래킹·suppression에서 제외"라고 하는데
   `internal/sender/process.go` 는 suppression만 건너뜁니다(`d.Lane != store.LaneProbe`). 프로브 메일에 오픈 픽셀과
   클릭 재작성이 들어갑니다(프로브 본문에 링크는 없어 클릭 재작성은 무해, 픽셀은 들어감).
   같은 조건을 트래킹 블록에도 걸면 됩니다.
6. **`SendingDomain.OutboundIPs` 를 갱신하지 않습니다.** 관측 IP는 `ProbeRun.ObservedIP` 에만 남깁니다.
   도메인 행까지 쓰려면 낙관적 갱신 한 번이 더 필요하고, 그 소유권은 control에 두는 편이 맞아 보여 남겨 뒀습니다.

## 테스트

```
go test -race ./internal/probe/...
```

`memstore` 기반으로 트리거·회수·타임아웃·최악 값·DNS 연동·이벤트·주기를 모두 덮습니다.
헤더 파서는 `testdata/` 픽스처로 따로 돕니다.
