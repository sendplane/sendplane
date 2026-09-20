# internal/sender

claim → 렌더 → SMTP 루프. 설계 근거는 [architecture.md §4.1–4.2, §8, §9.2, §10, §16](../../docs/architecture.md),
[ADR-0002](../../docs/adr/0002-store-as-queue.md) · [ADR-0003](../../docs/adr/0003-delivery-attempt-model.md) · [ADR-0011](../../docs/adr/0011-tracking-and-unsubscribe.md).

## 루프 (§8.1)

```
Run
 ├─ heartbeatLoop      10s: Workers.Heartbeat + ListActive(now-30s) → 활성 레플리카 수
 └─ loop
     ├─ 3s: Provider.ActiveTenants + 언헬시 transport 프로브
     └─ pass()  테넌트 라운드로빈(커서 이동) × 레인(transactional → bulk → probe)
         claimInto: limit = min(레인 여유, 테넌트 여유, ClaimBatch)
           ├─ runningCampaigns(3s 캐시) → ClaimRequest.CampaignIDs
           ├─ 용량을 **미리 예약**하고 claim, 못 채운 만큼 즉시 반납
           └─ jobs 채널 → 레인 워커 풀
               process(d) → DeliveryResult → tenant batcher (100건 또는 1초)
```

`Claim` 전에 용량을 예약하기 때문에 두 번의 pass가 풀을 초과 구독할 수 없습니다.
종료 시에는 claim을 멈추고 레인 채널을 닫아 진행 중인 메일만 마무리한 뒤 배처를 flush 합니다.

## process(d)

1. 테넌트 설정(짧은 TTL 캐시, 행이 없으면 `DefaultTenantSettings`) → 캠페인 / MessageVersion(불변, 영구 캐시) / Sender / Transport / SendingDomain
2. suppression (`SuppressionEnabled` && lane ≠ probe) → `suppressed`
3. transport가 unhealthy면 delivery는 **queued 유지**(시도 미소모)
4. 수신거부 목적지: **수신자 변수(`Delivery.UnsubscribeURL` → `Vars["unsubscribe_url"]`) > 테넌트 Liquid 템플릿 > `Hooks.UnsubscribeURL`**
5. 모드별 링크 결정(아래 표) → `{{ unsubscribe_url }}` 바인딩에 **렌더 전에** 주입
6. `PrepareChain(version, recipient.locale, campaign.default_locale)` → `Render`
7. 클릭 재작성(링크마다 토큰) → 오픈 픽셀 삽입 (§9.2)
8. `Hooks.BeforeSend` — `ErrSkip` → `suppressed`, 그 밖의 에러 → transient(재시도 소모)
9. MIME 조립 + 헤더 인젝션 검사 + 선택적 DKIM 서명
10. 레이트리밋 대기(transport 버킷, (transport, 수신 도메인) 버킷)
11. 풀 커넥션으로 전송 → `Classify` → 250이면 **즉시 `MarkSent`**, 결과는 배치 `Complete`

### 수신거부 모드 (§9.2)

| mode | 본문 `{{ unsubscribe_url }}` | `List-Unsubscribe` | `List-Unsubscribe-Post` |
|---|---|---|---|
| `sendplane` (도메인·키 있음) | `https://{tracking_domain}/t/u/{token}` | 같은 URL | `List-Unsubscribe=One-Click` |
| `sendplane` (도메인 또는 키 없음) | 호스트 목적지 그대로 | 같은 URL | 없음 |
| `sendplane` (목적지 없음) | 없음 | 없음 | 없음 |
| `host` | 호스트 목적지 | 같은 URL | 없음 (호스트가 원클릭을 받는다고 선언하지 않았으므로) |
| `none` | 없음 | 없음 | 없음 |

## 파일

| 파일 | 내용 |
|---|---|
| `sender.go` | `Config`(+기본값), 레인 풀, 라운드로빈 pass, 하트비트, 프로브 |
| `process.go` | 수신자 1명 파이프라인, 수신거부 해석, 결과 조립 |
| `message.go` | MIME(go-mail), 헤더 화이트리스트/인젝션 검사, DKIM(go-msgauth) |
| `classify.go` | SMTP 코드·enhanced·본문 → `store.ErrorClass` 규칙 테이블 |
| `policy.go` | 재시도/백오프/지터, `IncrementAttempt`, auth → queued |
| `ratelimit.go` | 토큰 버킷 + 워커 수 분할 + AIMD |
| `pool.go` | transport별 커넥션 풀 |
| `results.go` | `Complete` 배치 커밋 |
| `health.go` | transport 서킷(cooldown/unhealthy) + 상태 영속화 |
| `cache.go` | 테넌트별 TTL 캐시, 복호화된 비밀 캐시 |
| `metrics.go` | `Metrics` 인터페이스 + `NopMetrics` |

## 에러 분류 테이블 (§4.2)

위에서부터 첫 매치가 이깁니다. 한 규칙 안에서 코드/enhanced/본문 조건은 **AND**, 각 조건의 항목들은 **OR**.

| 규칙 | 조건 | class |
|---|---|---|
| `auth.code` | 530, 534, 535, 538 | `auth` |
| `auth.enhanced` | 5.7.8 / 5.7.9 / 4.7.8 | `auth` |
| `ratelimit.421` | 421 | `rate_limited` |
| `ratelimit.4xx.text` | 450·451·452 + `rate\|too many\|too quickly\|throttl\|slow down\|try again later\|deferred due to` | `rate_limited` |
| `ratelimit.enhanced` | 4.7.0 / 4.7.28 / 4.2.1 | `rate_limited` |
| `policy.enhanced` | 5.7.* | `policy` |
| `policy.text` | 5xx + `spam\|blocked\|blacklist\|blocklist\|reputation\|policy\|abuse` | `policy` |
| `permanent.5xx` | 5xx | `permanent` |
| `transient.4xx` | 4xx | `transient` |
| `auth.tls` | 인증서 검증 실패, TLS 레코드 오류 | `auth` |
| `transient.timeout` / `transient.connection` | 타임아웃, EOF, `*net.OpError` | `transient` |
| `transient.unknown` | 그 외 | `transient` |

`auth` 가 `policy` 보다 위인 것이 중요합니다 — 535는 enhanced가 5.7.8이라 순서를 바꾸면 policy로 잘못 분류됩니다.

문서는 이 표를 YAML로 두자고 했지만 **Go 리터럴**로 두었습니다: 어차피 컴파일되어야 하고,
규칙과 테스트가 같은 디렉터리에서 함께 움직이는 편이 낫습니다. 테넌트별 오버라이드가 필요해지면
같은 행 구조를 YAML에서 읽어 넣으면 됩니다.

## 정책 (§4.1, ADR-0003)

- `transient` / `rate_limited` 만 시도를 소모. 백오프는 테넌트 `RetryPolicy` + 지터 ±20%, 마지막 항목 재사용.
- `rate_limited` 는 `RateLimitCap`(기본 5분)으로 상한 — 실제 간격 조절은 레이트리미터가 합니다.
- `permanent` / `policy` → `failed`, 시도 미소모.
- `auth` → **delivery는 `queued` 유지**, 시도 미소모, `AuthRetryAfter`(기본 1분) 뒤 재적격 + transport `unhealthy` 마킹.
- 렌더 실패·헤더 인젝션·MIME 실패 → `failed`(permanent). 같은 입력이면 재시도해도 같은 결과입니다.
- 스토어 읽기 실패 등 **sender 자신의 문제**는 `queued` + 30초, 시도 미소모.

## 레이트리밋 (§8.2)

`share = transport.RatePerSecond / 활성 sender 레플리카 수`.
레플리카 수는 하트비트(10s)로 갱신되므로 스케일 변화에 1주기 내로 수렴합니다.
두 버킷(transport, (transport, 수신 도메인))을 **같은 시각으로 전진**시켜 한쪽이 기다리는 동안 다른 쪽 토큰을 태우지 않습니다.
`rate_limited` 응답 → 해당 버킷 절반(하한 `share/64`), 이후 **분당 +10%** 로 share까지 회복(AIMD).

## 루트 패키지와의 연결

이 패키지는 `sendplane` 루트를 import하지 **않습니다**. 루트의 `RunSender` 가 여기를 호출할 예정이라
반대 방향 의존이 순환이 되기 때문입니다. 그래서 `Hooks` / `OutboundMessage` / `RecipientContext` /
`SecretCipher` / `ErrSkip` 을 여기에 두었고, 루트는 얇은 어댑터를 씁니다:

```go
cfg := sender.Config{
    WorkerID: c.WorkerID,
    Secrets:  s.opts.Secrets,           // 인터페이스가 구조적으로 동일 → 그대로 대입
    Hooks: sender.Hooks{
        UnsubscribeURL: func(ctx context.Context, rc sender.RecipientContext) (string, error) {
            return s.opts.Hooks.UnsubscribeURL(ctx, sendplane.RecipientContext(rc))
        },
        BeforeSend: func(ctx context.Context, m *sender.OutboundMessage) error {
            err := s.opts.Hooks.BeforeSend(ctx, (*sendplane.OutboundMessage)(m))
            if errors.Is(err, sendplane.ErrSkip) {
                return sender.ErrSkip
            }
            return err
        },
    },
}
```

`sendplane.RecipientContext` / `OutboundMessage` 를 `= sender.X` 타입 별칭으로 바꾸면 어댑터도 필요 없습니다.

## 라이브러리

| 용도 | 선택 | 메모 |
|---|---|---|
| MIME 빌더 + SMTP 클라이언트 | `github.com/wneessen/go-mail` v0.8.1 | 메시지는 `mail.Msg` 로 만들고 **`WriteTo` 로 바이트를 얻은 뒤** `go-mail/smtp` 클라이언트로 직접 MAIL/RCPT/DATA 합니다. `mail.Client` 를 쓰지 않는 이유: envelope sender(VERP)와 커넥션 수명·재사용을 sender가 직접 통제해야 하고, DKIM 서명을 완성된 바이트에 적용해야 하기 때문 |
| DKIM | `github.com/emersion/go-msgauth/dkim` v0.7.0 | `dkim.Sign(w, r, opts)`, relaxed/relaxed, RSA·Ed25519(PKCS#1/PKCS#8 PEM) |
