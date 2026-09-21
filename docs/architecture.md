# sendplane 아키텍처 설계

> 상태: v3 (2026-09-21, 구현 반영). 결정 근거는 [adr/](adr/) 참조, 구현 순서는 [roadmap.md](roadmap.md) 참조.
> 이 문서는 코드와 어긋난 부분을 실제 구현(`sendplane.go`, `options.go`, `host/*.go`, `internal/*/README.md`, `store/*/README.md`, `test/{load,e2e}/README.md`, `.github/workflows/*.yml`)에 맞춰 바로잡은 버전입니다.

## 0. 한 줄 요약

sendplane은 **"메일 발송 엔진 라이브러리 + 참조 바이너리"** 입니다. listmonk의 발송 엔진, Keila의 콘텐츠 모델을 가져오되,
계정/구독자/수신거부 같은 "제품" 기능은 의도적으로 **호스트 애플리케이션에 위임**합니다.
호스트는 `sendplane.New(opts)`로 인증·권한·테넌트·수신거부 URL·이벤트 수신을 주입하고,
메일 주소는 API 호출 시점에 스트리밍으로 넘깁니다. 내부는 `Campaign → MessageVersion → Delivery → DeliveryAttempt`
를 1급 엔티티로 두어 메일 단위 재시도, 바운스 상관관계, 감사 추적이 자연스럽게 나오도록 설계합니다.

## 1. 요구사항 정리와 해석

| 요구사항 | 해석 / 설계상 결론 |
|---|---|
| Template / Transactional / Campaign / Bounce | 내부 파이프라인은 하나(Delivery). Campaign과 Transactional은 **진입점(API)과 스케줄링 레인만 다름** |
| 사용자별 i18n | 수신자 단위 `locale` + 템플릿 단위 번역 번들. 렌더는 sender에서 수신자별로 수행 |
| 계정/RBAC 없음, 임베딩 가능 | `Authenticator`, `Authorizer`, `TenantResolver` 인터페이스 주입. 참조 바이너리는 API Key/JWT 구현체 제공 |
| 수신거부는 호스트가 구현, URL은 sendplane | 기본 모드: 메일에는 **sendplane 트래킹 URL**이 들어가고 클릭을 기록한 뒤 호스트 URL로 리다이렉트(원클릭 POST는 호스트에 이벤트 통지). 대안 모드: 호스트 URL을 직접 넣고 호스트가 API로 수신거부 사실을 알림. 두 경우 모두 캠페인별 수신거부율 산출 (§9) |
| 오픈/클릭 트래킹 | 픽셀 + 서명된 리다이렉트 링크. 테넌트/캠페인 단위 on/off, 봇 클릭 표시 (§9) |
| email DB 없음, 1M+ 수신자 | 수신자는 캠페인에 **NDJSON 스트리밍**으로 append. 캠페인 종속 데이터(Delivery)로만 존재, 캠페인 보존기간 후 삭제 |
| control과 sender 분리, k8s | 두 프로세스는 **DB(store)만 공유**하고 직접 통신하지 않음. Redis/NATS 필수 의존성 없음 |
| 다중 DB (pg/mongo/커스텀) | `store` 패키지의 리포지터리 인터페이스 + `storetest` 적합성 스위트. 다중 문서 트랜잭션에 의존하지 않는 상태 전이 설계 |
| 블록 위지윅 + HTML | 블록 편집기(GrapesJS-MJML) → MJML → HTML은 **publish 시점에 1회 컴파일**. 발송 시엔 Liquid만 렌더 |
| 메일 단위 재시도 | `Delivery.status` 상태기계 + `DeliveryAttempt` 이력 + 에러 분류(transient/permanent/rate_limited/auth/policy) |
| 멀티테넌시 (컬럼 or 별도 커넥션) | `store.Provider.ForTenant(tenantID)`: 기본 구현은 `tenant_id` 컬럼, 대안 구현은 테넌트→DSN 라우팅 |
| SPF/DKIM/DMARC/PTR 주기 점검 | **루프백 프로브**: 실제로 메일을 보내 프로브 메일박스(IMAP)에서 수신 후 `Authentication-Results`/`Received` 헤더로 판정. DNS 정적 검사는 원인 진단용 보조 계층 (§11) |
| Go / Vue / SMTP / IMAP·POP3 | 아래 기술 선택 참조 |
| CI: pg/mongo 동일 기능, 1M 부하 | storetest 매트릭스 + chaos-smtp 기반 1M 워크플로우 (§15) |

**의도적으로 하지 않는 것 (범위 밖)**: 구독자/연락처 DB, 세그먼트, 가입 폼, 사용자·역할 관리, 수신거부/선호 센터 **페이지**(수신거부 링크의 목적지는 호스트, §9).

## 2. 시스템 구성

```
                 ┌──────────────────────────────────────────────────────────┐
                 │ Host application (Go 임베딩 또는 참조 바이너리 + HTTP)       │
                 │  - Authenticator / Authorizer / TenantResolver            │
                 │  - unsubscribe URL 제공, 이벤트(webhook/Go hook) 수신        │
                 └───────────────┬──────────────────────────────────────────┘
                                 │ REST (/api/v1), Vue console(@sendplane/ui)
                 ┌───────────────▼──────────────────────────────────────────┐
                 │ sendplane-control  (Deployment, N replicas)              │
                 │  HTTP API · 템플릿/캠페인 관리 · 수신자 인제스트 · MJML 컴파일  │
                 │  Leader loop(스토어 lease): 스케줄러, 완료 판정, 통계 집계,   │
                 │  도메인 헬스체크, 보존기간 정리, 만료 lease 회수, 이벤트 아웃박스  │
                 └───────────────┬──────────────────────────────────────────┘
                                 │  store.Provider (Postgres | MongoDB | custom)
                                 │  ── 유일한 공유 지점, 큐 역할 포함 ──
   ┌─────────────────────────────┼─────────────────────────────┐
   │                             │                             │
┌──▼──────────────────────┐ ┌────▼────────────────────────┐ ┌──▼──────────────────────┐
│ sendplane-sender (HPA)  │ │ sendplane-sender (HPA)      │ │ sendplane-bounce (1/mbox)│
│ claim → render → SMTP   │ │ lanes: transactional / bulk │ │ IMAP/POP3 poll → DSN/ARF │
│ retry 정책, rate limit   │ │ transport pool, circuit brk │ │ parse → delivery 상태갱신 │
└──────────┬──────────────┘ └────────────┬────────────────┘ └────────────┬────────────┘
           │        SMTP (relay / 직접)    │                              │ IMAP/POP3
           ▼                              ▼                              ▼
       Mail servers                   Mail servers                  Bounce mailbox
```

프로세스 역할은 플래그로 조합합니다: `sendplane --roles=control,sender,bounce` (개발/소규모 단일 바이너리),
k8s에서는 역할별 Deployment. **모든 역할이 같은 Go 라이브러리를 링크**하므로 호스트가 Go 훅을 쓰는 경우
호스트가 자신의 `cmd/`에서 세 역할을 빌드하면 됩니다(ADR-0001).

### 2.1 Go 모듈 레이아웃

```
github.com/sendplane/sendplane
├── sendplane.go            // New(), Handler(), RunControl(), RunSender()  ← 공개 API
├── options.go              // Options, host 타입 별칭, Action 상수
├── bounce.go / probe.go    // RunBounce/RunControl이 internal/bounce, internal/probe에 배선하는 어댑터
├── host/                   // 호스트가 주입하거나 받는 타입들 (공개, leaf)
│                           //   Principal/Action/Authenticator/Authorizer/TenantResolver,
│                           //   Hooks/Event/EventSink/OutboundMessage/RecipientContext,
│                           //   SecretCipher, Limits, Metrics, ProbeConfig
├── store/                  // 리포지터리 인터페이스 + 모델 (공개: 커스텀 DB 구현용)
│   ├── postgres/           // pgx 기반 구현 + migrations/
│   ├── mongo/              // 공식 mongo-driver/v2 기반 구현 + index bootstrap
│   ├── memstore/           // 인메모리 구현(테스트 전용)
│   └── storetest/          // 적합성 스위트: storetest.Run(t, newStore), 54개 서브테스트
├── cmd/
│   ├── sendplane/          // 참조 바이너리 (--roles, 설정 파일 기반 auth/hook 구현)
│   │   └── console/        // web/apps/console/dist를 //go:embed하는 leaf 패키지(ADR-0010)
│   └── chaos-smtp/         // 테스트용 결정적 실패 주입 SMTP 서버(internal/chaossmtp 래핑)
├── internal/
│   ├── api/                // HTTP 핸들러(chi + strict server), OpenAPI(api/openapi.yaml) 85개 오퍼레이션과 1:1
│   ├── control/             // 리더 루프 7종 + TrackingBuffer + 캠페인 상태기계(ADR-0002, ADR-0003)
│   ├── ingest/               // NDJSON 수신자 스트리밍 인제스트, CSV→NDJSON 변환
│   ├── render/                // Liquid 엔진, i18n 태그, MJML 컴파일, HTML→text 자체 구현
│   ├── sender/                 // claim loop, transport pool, rate limiter, retry policy, error classifier
│   ├── bounce/                  // IMAP/POP3 poller, DSN(RFC3464)/ARF(RFC5965) 파서, VERP 상관관계
│   ├── mailbox/                  // bounce·probe가 공유하는 IMAP/POP3 수신 클라이언트
│   ├── probe/                     // 루프백 발신 헬스 체크 러너(ADR-0012)
│   ├── dnscheck/                   // SPF/DKIM/DMARC/MX/PTR 진단 계층
│   ├── tracking/                    // 무상태 서명 토큰(오픈/클릭/수신거부) + VERP 주소
│   └── chaossmtp/                    // 결정적으로 실패하는 인프로세스 ESMTP 서버(테스트 인프라)
├── api/openapi.yaml        // 단일 진실 원천. Go 서버 타입과 TS 클라이언트를 여기서 생성
├── web/                    // pnpm workspace: packages/api, packages/ui, apps/console (§13)
├── deploy/
│   ├── dev/                // 로컬 docker-compose(postgres/mongo) + 참조 이미지 Dockerfile
│   └── helm/sendplane/     // control/sender/bounce/migrate 차트
├── test/
│   ├── load/               // 1M 수신자 부하 테스트 하니스(§15.1)
│   └── e2e/                // compose 기반 종단 간 시나리오 하니스(§15)
└── docs/
```

공개 패키지는 `sendplane`(루트), `host`, `store`(및 `store/postgres`, `store/mongo`)뿐입니다. `internal/*`는 Hyrum's Law 표면을 최소화하려고 막아 둡니다.

`host`가 따로 있는 이유는 **import 사이클** 하나뿐입니다. 루트는 `Handler`/`RunControl`/`RunSender`/`RunBounce`를 구현하려고
`internal/api`·`internal/control`·`internal/sender`·`internal/bounce`·`internal/probe`를 import하는데, 그 패키지들도 호스트가 주입하는 타입(`Hooks`, `Limits`,
`EventSink`, `Metrics`, `ProbeConfig` …)이 필요합니다. 그래서 그 타입들은 `store`만 import하는 leaf 패키지 `host`에 두고,
루트(`options.go`)가 **타입 별칭**으로 전부 재노출합니다(`type Hooks = host.Hooks`). 호스트는 계속 `sendplane.X`만 쓰면 되고,
`sendplane.Hooks`와 `host.Hooks`는 같은 타입이라 경계에서 변환이 필요 없습니다.

## 3. 임베딩 API (`sendplane.New`)

`Options`·`Hooks`·`SenderConfig`·`ProbeConfig`의 실제 선언은 `options.go`, `sendplane.go`, `host/*.go`에 있습니다. 아래는 그 요지입니다.

```go
package sendplane // options.go

type Options struct {
    Store   store.Provider // 필수
    Auth    Authenticator  // 필수. 요청 → Principal
    Authz   Authorizer     // 선택. 기본: 인증된 principal은 전부 허용
    Tenants TenantResolver // 선택. 기본: Principal.TenantID, 없으면 "default"
    Hooks   Hooks
    Secrets SecretCipher // SMTP/IMAP 비밀번호, DKIM 키 at-rest 암호화
    Limits  Limits       // 0인 필드는 DefaultLimits로 채워짐
    // Probe는 루프백 헬스 프로브(§11)를 켠다. 기본 off: 프로브 메일박스가
    // 없는데 트리거만 도는 것은 아예 없는 것보다 나쁘다.
    Probe   ProbeConfig
    Logger  *slog.Logger      // 기본: slog.Default()
    Clock   func() time.Time
    Metrics Metrics           // sender가 방출하는 카운터/히스토그램. 기본: host.NopMetrics
}

// 아래 타입은 전부 leaf 패키지 host에 선언되어 있고 루트가 별칭으로 재노출합니다(§2.1).
// sendplane.Hooks와 host.Hooks는 같은 타입입니다.
type (
    Principal         = host.Principal
    Authenticator      = host.Authenticator // Authenticate(r) (*Principal, error), 실패 시 ErrUnauthenticated
    Action              = host.Action        // x-sendplane-action 값과 1:1인 폐쇄 집합(예: ActionCampaignSend, ActionMessageSend)
    Resource             = host.Resource      // {Kind, ID, TenantID}
    Authorizer            = host.Authorizer    // Authorize(ctx, p, a, r) error, 실패 시 ErrForbidden
    TenantResolver         = host.TenantResolver
    SecretCipher            = host.SecretCipher
    ProbeConfig               = host.ProbeConfig // Enabled, HMACKey, Nameservers, Interval, Timeout
    Hooks                      = host.Hooks
    RecipientContext            = host.RecipientContext // 훅에 넘어가는 수신자별 데이터
    OutboundMessage               = host.OutboundMessage  // BeforeSend가 보는 렌더된 메시지
    UnsubscribeNotice              = host.UnsubscribeNotice
    EventSink                       = host.EventSink
    Limits                           = host.Limits
    Metrics                           = host.Metrics
)

type Hooks struct { // host/hooks.go
    // 수신거부 "목적지"(호스트 URL). 우선순위: 수신자 unsubscribe_url 변수 > 테넌트 URL 템플릿 > 이 훅.
    UnsubscribeURL func(ctx context.Context, rc RecipientContext) (string, error)
    // RFC 8058 원클릭 수신거부가 sendplane 엔드포인트로 들어왔을 때 동기 통지. nil이거나 실패하면 이벤트(webhook)로 재시도.
    Unsubscribed func(ctx context.Context, u UnsubscribeNotice) error
    // 발송 직전 거부/수정. ErrSkip 반환 시 delivery는 suppressed.
    BeforeSend func(ctx context.Context, m *OutboundMessage) error
    Events EventSink // 기본: 아웃박스 → 테넌트 설정의 webhook URL
}

func New(o Options) (*Sendplane, error)
func (s *Sendplane) Handler() http.Handler                 // /api/v1, /t/*, /healthz. 호스트 mux에 mount
func (s *Sendplane) RunControl(ctx context.Context) error  // 리더 루프 + TrackingBuffer
func (s *Sendplane) RunSender(ctx context.Context, c SenderConfig) error
func (s *Sendplane) RunBounce(ctx context.Context, c BounceConfig) error

// SenderConfig configures one sender process (sendplane.go). WorkerID는 필수
// (lease owner이자 heartbeat 행의 ID), 나머지는 전부 기본값이 있다.
type SenderConfig struct {
    WorkerID             string
    Lanes                map[store.Lane]int // 기본 transactional 8, bulk 32
    ClaimBatch           int
    LeaseFor             time.Duration
    PollInterval         time.Duration
    CampaignRefresh      time.Duration // running-campaign 집합의 TTL(ADR-0002)
    TenantConcurrency    int
    DefaultRatePerSecond float64
    MaxMsgsPerConn       int
    EHLOName             string
    TLSConfig            *tls.Config
}
```

- **Go 코드 없이도 통합 가능**해야 합니다(비-Go 호스트). 그래서 수신거부 URL은 "수신자 변수" 또는
  "테넌트 URL 템플릿(Liquid, 예: `https://app.example.com/u?e={{ recipient.email | url_encode }}&t={{ recipient.vars.unsub_token }}`)"
  으로도 줄 수 있고, 이벤트는 webhook으로 받을 수 있습니다. Go 훅은 escape hatch입니다.
- 참조 바이너리(`cmd/sendplane`)는 `Authenticator`로 **정적 API Key(테넌트 매핑 포함) / JWT(JWKS)** 두 구현을 설정 파일로 제공합니다.
- `Handler()`와 `RunControl()`은 프로세스 전체에서 공유하는 하나의 `*control.Control`을 처음 호출한 쪽이 만듭니다(`sendplane.controlPlane`, `sync.Once`). `Options.Probe.Enabled`가 켜져 있으면 이때 `probe-trigger`/`probe-collect` 리더 루프도 함께 등록됩니다. `mailbox-check`(§11.5)는 `Probe.Enabled`와 무관하게 항상 등록됩니다 — 바운스 메일박스도 같은 감시가 필요합니다.

## 4. 도메인 모델

```
Tenant (설정만: 재시도 정책, 보존기간, suppression on/off, unsubscribe URL 템플릿, 기본 locale)
 ├─ Transport        SMTP 계정 (host/port/tls/auth, max_conns, rate, per-domain limit, 상태)
 ├─ Sender           From 아이덴티티 (name, email, reply_to, transport_id, domain_id)
 ├─ SendingDomain    DKIM selector/key(선택), return-path 도메인, 기대 SPF, 아웃바운드 IP, 헬스 이력
 ├─ BounceMailbox    IMAP/POP3 설정
 ├─ ProbeMailbox     루프백 헬스체크용 수신 메일박스(IMAP, 외부 프로바이더 계정 권장, 복수 가능)
 ├─ TrackingConfig   tracking_domain, opens/clicks on/off, unsubscribe_mode(sendplane|host|none), 서명 키
 ├─ Layout           MJML|HTML + {{ content }} 슬롯 + 자체 i18n 번들
 ├─ Template         subject/preheader/body(blocks|mjml|html), text(opt), i18n 번들, layout_id
 │    └─ MessageVersion   ← 불변 스냅샷: 컴파일된 html/text/subject Liquid + 병합된 i18n + default_locale
 ├─ Campaign         message_version_id, sender_id, default_locale, 공통 vars, 일정, 상태, 통계 캐시
 │    ├─ RecipientChunk   인제스트 멱등성 단위 (chunk_key, count, state)
 │    └─ Delivery ──┐
 ├─ Delivery ◄──────┘  (campaign_id nullable: transactional)
 │    └─ DeliveryAttempt      (Delivery에는 first_opened_at / first_clicked_at / unsubscribed_at 요약 컬럼)
 ├─ Suppression      (선택 기능) email_norm, reason, source_delivery_id, expires_at
 ├─ BounceEvent      raw + 파싱 결과 + 상관관계 근거
 ├─ TrackingEvent    open|click|unsubscribe_clicked|unsubscribed, delivery_id, url, ua, suspected_bot
 ├─ ProbeRun         Sender별 루프백 결과(수신 여부, 지연, SPF/DKIM/DMARC 판정, TLS, PTR, 도착 폴더) + DNS 검사 결과
 └─ EventOutbox      호스트로 나갈 이벤트, 전송 상태
```

핵심 불변식:
`Campaign ≠ MessageVersion ≠ Delivery ≠ DeliveryAttempt`, `Template ≠ MessageVersion(발송된 것)`.

### 4.1 Delivery 상태기계

```
pending ──(campaign start: running-set 진입)──► queued ──claim──► leased ──SMTP 250──► sent ──(DSN)──► bounced | complained
   │                                              ▲               │
   │                                              │  transient    ├─ 4xx / conn err ──► deferred ──(next_attempt_at)──┘
   │                                              └───────────────┘
   │                                                              ├─ 5xx / max attempts ──► failed
   │                                                              └─ BeforeSend ErrSkip / suppression ──► suppressed
   └──(campaign cancel)──► cancelled        (queued/deferred/pending 에서만; leased는 완료 후 cancel 무시)
```

- 상태 값: `pending, queued, leased, deferred, sent, failed, bounced, complained, suppressed, cancelled`.
  `bounced`는 `sent` 이후 비동기 DSN으로만 진입(hard bounce). soft bounce는 `BounceEvent`로 기록하고 상태는 유지.
- **재시도 카운트는 transient에만 소모**. transport 자체 문제(auth 실패, 연결 거부)는 attempt를 기록하되 `attempt_count`를 늘리지 않고 transport를 cooldown 시킴.
- **수동 재시도**: `POST /campaigns/{id}/retry {filter}` 또는 `POST /deliveries/{id}/retry` → `retry_generation++`, `attempt_count` 유지, `queued`로 복귀. 이력이 끊기지 않음.
- **`pending → queued` 화살표는 개념도입니다. 실제로는 행을 갱신하지 않습니다** (ADR-0002 보완, §7.2). `DeliveryRepo.Claim`은 `queued`/`deferred`뿐 아니라, 호출자가 넘긴 running 캠페인 집합에 속한 캠페인의 `pending` 행도 **그 자리에서 직접** `leased`로 올립니다(`store/postgres/delivery.go`의 `Claim`: `status IN (1,3) OR (status = 0 AND campaign_id = ANY(...))`). campaign 필터가 아예 없는 호출(`CampaignIDs == nil`)에는 `pending` 행이 전혀 섞이지 않습니다 — 그래서 캠페인이 `running`이 아닌 한 인제스트만 끝난 1M 행이 새어나가지 않습니다. `start`가 쓰는 행은 캠페인 하나뿐입니다.

### 4.2 에러 분류 (normalize)

| class | 판정 | 처리 |
|---|---|---|
| `transient` | 4xx (아래 제외), 연결/타임아웃, 4.x.x enhanced code | 지수 백오프 재시도 (기본 1m·5m·15m·1h·4h·12h, jitter, 최대 6회 / 24h) |
| `rate_limited` | 421, 450/451 + "rate|too many|throttl", 4.7.0/4.7.28 | 짧은 재시도 + **transport 레벨 슬로다운**(토큰 버킷 축소, 최대 N분 cooldown) |
| `permanent` | 5xx (아래 제외), 5.1.x | `failed`, suppression 후보 아님(사용자 미존재는 bounce 경로에서 별도) |
| `policy` | 5.7.x, 550 + "spam|blocked|blacklist" | `failed` + 이벤트 severity 상승 + 도메인 헬스 재검사 트리거 |
| `auth`/`config` | 530/535, TLS handshake 실패, EHLO 거부 | delivery는 `queued` 유지, **transport unhealthy** 마킹, 알림 이벤트 |

분류 테이블은 데이터(YAML)로 두고 테스트 픽스처와 공유합니다.

## 5. 스토어 계층

### 5.1 인터페이스 (요지)

```go
package store // store/store.go

type Provider interface {
    ForTenant(ctx context.Context, tenantID string) (Store, error)
    // ActiveTenants: 비종료 delivery가 있거나 scheduled|running|paused 캠페인이 있는
    // 테넌트. sender와 대부분의 리더 루프가 폴링 대상으로 쓴다. SystemTenantID는 절대 포함하지 않음.
    ActiveTenants(ctx context.Context) ([]string, error)
    // Tenants: 활성 여부와 무관하게 설정 행이나 설정성 행(transport/sender/도메인/
    // bounce·probe mailbox/layout/template/campaign)이 있는 전체 테넌트. 바운스 폴러와
    // control의 AllTenants 루프(§7.3, §10, §11.2, §12)가 이걸 쓴다 — 바운스와 프로브
    // 판정은 테넌트가 이미 한가해진 뒤에 도착한다.
    Tenants(ctx context.Context) ([]string, error)
    Migrate(ctx context.Context) error
    Close() error
}

type Store interface {
    TenantSettings()  TenantSettingsRepo
    Transports()      TransportRepo
    Senders()         SenderRepo
    Domains()         DomainRepo
    BounceMailboxes() BounceMailboxRepo // 테넌트 리소스, /api/v1/bounce-mailboxes (§10)
    ProbeMailboxes()  ProbeMailboxRepo  // 테넌트 리소스, /api/v1/probe-mailboxes (§11.1)
    ProbeRuns()       ProbeRunRepo
    Layouts()         LayoutRepo
    Templates()       TemplateRepo // + i18n bundle
    Versions()        MessageVersionRepo
    Campaigns()       CampaignRepo
    RecipientChunks() RecipientChunkRepo
    Deliveries()      DeliveryRepo
    Attempts()        AttemptRepo
    Suppressions()    SuppressionRepo
    Bounces()         BounceRepo
    Tracking()        TrackingRepo
    Outbox()          OutboxRepo
    Locks()           LockRepo   // 리더 선출 / 싱글턴 잡 lease
    Workers()         WorkerRepo // sender heartbeat (rate 분배용)
}

// 큐 역할을 하는 핵심 메서드
type DeliveryRepo interface {
    InsertBatch(ctx, []Delivery) (inserted int, err error)        // (campaign_id, email_norm) 유니크로 멱등
    Claim(ctx, ClaimRequest) ([]Delivery, error)
    Complete(ctx, []DeliveryResult) error                          // 상태 전이 + attempt insert, 배치
    ReleaseExpiredLeases(ctx, now time.Time, limit int) (int, error)
    CountByStatus(ctx, campaignID) (map[Status]int64, error)
    BulkTransition(ctx, campaignID, from []Status, to Status, limit int) (int, error) // cancel 등, 청크 반복
    MarkBounced(ctx, id, now) (changed bool, err error)  // lease 없는 비동기 DSN 전이, status=sent CAS
    MarkComplained(ctx, id, now) (changed bool, err error)
    ...
}
type ClaimRequest struct {
    Lane        Lane      // transactional | bulk | probe
    CampaignIDs []string  // nil: pending 없이 queued/deferred만. len 0: campaign_id IS NULL만. 값 있음: 그 캠페인들의 pending도 포함(§4.1)
    Limit       int
    LeaseFor    time.Duration
    WorkerID    string
    Now         time.Time // claim 기준 시각. queued/deferred는 NextAttemptAt <= Now인 것만, lease는 Now+LeaseFor까지
}
```

시간 해상도는 **밀리초**입니다: `store.TruncateTime`이 쓰기 직전 모든 `time.Time`을 밀리초로 절삭하고(반올림이 아니라 절삭), `Claim`의 `next_attempt_at <= now` 같은 비교 경계값도 같은 함수를 지나므로 같은 값에서 만든 시각끼리는 항상 정확히 일치합니다(`store/doc.go`). Postgres는 `timestamptz`를 그대로 절삭해 쓰고, Mongo는 BSON date의 네이티브 해상도가 이미 밀리초라 자연히 맞습니다.

설계 규칙:
- **다중 리포지터리 트랜잭션을 요구하지 않음.** 원자성이 필요한 곳은 조건부 갱신(CAS)과 멱등 삽입으로 해결한다(Mongo, 커스텀 DB 친화).
- 모든 메서드는 `Store`가 이미 테넌트에 바인딩된 상태이므로 tenantID 인자를 받지 않는다. shared 구현이 내부적으로 `tenant_id` 조건을 강제 → 테넌트 누락 버그를 구조적으로 차단.
- `storetest.Run(t, factory)`가 계약이다: 멱등 삽입, 동시 claim 시 중복 없음, lease 만료 회수, 상태 전이 CAS, 테넌트 격리(다른 테넌트 Store로는 `ErrNotFound`), 커서 페이지네이션 안정성 등 **54개 서브테스트**를 두 구현(및 memstore)에 동일하게 실행한다(ADR-0007).

### 5.2 Postgres 구현 요점

```sql
CREATE TABLE delivery (
  id            uuid PRIMARY KEY,               -- UUIDv7 (시간 정렬)
  tenant_id     text NOT NULL,
  campaign_id   uuid,                           -- NULL = transactional
  version_id    uuid NOT NULL,
  lane          smallint NOT NULL,              -- 0 bulk, 1 transactional
  priority      smallint NOT NULL DEFAULT 0,
  status        smallint NOT NULL,
  email         text NOT NULL,
  email_norm    text NOT NULL,
  locale        text,
  vars          jsonb,
  unsubscribe_url text,
  attempt_count smallint NOT NULL DEFAULT 0,
  retry_gen     smallint NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  lease_owner   text, lease_until timestamptz,
  last_error_class smallint, last_smtp_code smallint, last_error text,
  message_id    text, sent_at timestamptz, finished_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX delivery_campaign_email ON delivery (campaign_id, email_norm) WHERE campaign_id IS NOT NULL;
CREATE INDEX delivery_claim ON delivery (tenant_id, lane, next_attempt_at) WHERE status IN (1 /*queued*/, 3 /*deferred*/);
CREATE INDEX delivery_lease ON delivery (lease_until) WHERE status = 2 /*leased*/;
CREATE INDEX delivery_campaign_status ON delivery (campaign_id, status);
```

claim:
```sql
WITH c AS (
  SELECT id FROM delivery
   WHERE tenant_id = $1 AND lane = $2 AND status IN (1,3) AND next_attempt_at <= now()
     AND (campaign_id IS NULL OR campaign_id = ANY($3))
   ORDER BY priority DESC, next_attempt_at
   LIMIT $4 FOR UPDATE SKIP LOCKED)
UPDATE delivery d SET status = 2, lease_owner = $5, lease_until = now() + $6, updated_at = now()
  FROM c WHERE d.id = c.id RETURNING d.*;
```

- 인제스트는 `COPY ... FROM STDIN` 임시 테이블 → `INSERT ... ON CONFLICT DO NOTHING` 또는 multirow INSERT(2,000행 단위).
- `Complete`는 500건 단위 `UPDATE ... FROM unnest($1::uuid[], ...)` + attempt `COPY`.
- 파티셔닝(월 단위 `created_at`)은 보존기간 삭제가 문제될 때 도입. 처음엔 인덱스 삭제 배치로 충분.

### 5.3 MongoDB 구현 요점

- 컬렉션은 테이블과 1:1. `delivery`에 `{campaign_id:1, email_norm:1}` unique(partial), `{tenant_id, lane, status, next_attempt_at}` 복합 인덱스.
- claim: 후보 `_id` N개 `find` → `updateMany({_id:{$in}, status:{$in:[queued,deferred]}}, {$set:{status:leased, lease_owner, lease_until}})` → `find({lease_owner: token, status: leased})`. 경합에서 일부를 놓치는 것은 허용(다음 루프).
- 인제스트: `insertMany(ordered:false)`, 중복 키 오류는 무시하고 건수만 집계.
- 트랜잭션 미사용(standalone 인스턴스에서도 동작).

### 5.4 멀티테넌시 모드

| 모드 | Provider 구현 | 용도 |
|---|---|---|
| shared (기본) | `store/postgres.NewShared(pool)` → 모든 테이블 `tenant_id` 컬럼, `ForTenant`는 스코프 래퍼 반환 | 대부분의 SaaS |
| routed | `store.NewRouted(func(tenantID) (Provider, error))` + 커넥션 캐시(LRU, idle close) | 테넌트별 DB 격리 요구, 규제 |
| 혼합 | routed가 "그 외 테넌트"를 shared로 폴백 | 대형 고객만 격리 |

sender는 `Provider.ActiveTenants()`를 주기적으로 갱신해 테넌트를 라운드로빈하며 **테넌트별 동시성 상한**을 둡니다(한 테넌트의 1M 캠페인이 다른 테넌트의 transactional을 굶기지 않게).
Postgres shared 모드에서 RLS(row-level security)는 선택 강화 항목으로 남겨둡니다.

## 6. 콘텐츠 · i18n · 렌더링

### 6.1 템플릿 표현식: Liquid

- 엔진: Go `osteele/liquid`(서버) + `liquidjs`(에디터 미리보기 보조). **표준 필터 + 아래 커스텀만** 사용하고 `include/render/layout` 태그는 비활성화.
- 변수: `{{ recipient.name }}`, `{{ vars.order.total }}`, `{{ campaign.name }}`, `{{ unsubscribe_url }}`, `{{ locale }}`.
- i18n 태그: `{% t "welcome.title" %}` / 인자 전달 `{% t "greeting" name: recipient.name %}` / 필터형 `{{ "welcome.title" | t }}`.
  번역 문자열 자체도 Liquid로 해석되므로 `"{{ name }}님, 환영합니다"` 처럼 변수 사용 가능.
- **HTML 파트는 기본 이스케이프**: 바인딩의 문자열 값을 렌더 전에 재귀적으로 HTML 이스케이프. 신뢰된 HTML 조각은 API에서 `{"$html": "<b>..</b>"}` 형태로만 전달 가능. subject/text 파트는 이스케이프 없음. (ADR-0004)
- 요청서의 `{user.name}` 단일 중괄호 대신 Liquid 이중 중괄호를 채택한 이유는 ADR-0004 참조. 사용자 확인 완료(ADR-0004 상태 참조).

### 6.2 i18n 번들

```yaml
# GET /api/v1/templates/{id}/i18n?format=yaml  /  PUT 동일 경로로 import
default_locale: en
locales:
  en:
    welcome.title: "Welcome, {{ recipient.name }}"
    cta.label: "Open dashboard"
  ko:
    welcome.title: "{{ recipient.name }}님, 환영합니다"
    cta.label: "대시보드 열기"
  ko-KR: {}          # 비어 있으면 ko로 폴백
```

- 폴백 체인: `recipient.locale` → 언어만(`ko-KR`→`ko`) → `campaign.default_locale` → `template.default_locale` → 키 문자열 그대로 + 경고 이벤트.
- Layout도 자체 번들을 가지며 publish 시 Template 번들과 병합(Template 우선)해 MessageVersion에 저장.
- 서버가 템플릿을 파싱해 사용 중인 키 목록을 반환(`GET .../i18n/keys`) → UI가 누락 키를 표시. import 시 미사용 키는 경고, 누락 키는 publish 차단(테넌트 설정으로 완화 가능).
- 복수형/성별 등 ICU MessageFormat은 초기 범위 밖. 필요 시 `count.one/count.other` 키 컨벤션으로 시작(§18).

### 6.3 콘텐츠 모드와 컴파일 파이프라인

```
Template.body
  ├─ blocks : GrapesJS-MJML project JSON  ─┐
  ├─ mjml   : MJML 소스                    ─┼─► [publish] Layout 슬롯 병합 → MJML 컴파일(mjml-go, WASM) ─► HTML(Liquid 포함)
  └─ html   : 원본 HTML                    ─┘                     (blocks/mjml 만)
                                              text 없음 → HTML→text 자동 생성
                                              결과: MessageVersion { subject_tpl, html_tpl, text_tpl, i18n, default_locale, checksum }
[send, per recipient]  locale 결정 → 파싱 캐시(version, locale) → Liquid 렌더 3종 → MIME 조립 → (DKIM 서명) → SMTP
```

- **MJML은 publish 시 1회 컴파일**하고 Liquid는 그 뒤에 수신자별로 실행합니다(Keila v0.30은 반대 순서로 바꿨지만 그 방식은 수신자마다 MJML 컴파일이 필요해 1M 규모에 부적합). MJML 구조 사이의 Liquid 제어문은 `<mj-raw>{% if %}</mj-raw>` 컨벤션을 사용하고, 블록 에디터는 "조건부 블록" 속성으로 이를 생성합니다.
- 컴파일 산출물은 불변. Template을 수정해도 진행 중 캠페인은 자기 MessageVersion을 씁니다. Transactional은 `template_id`로 호출하면 "현재 publish된 버전"을 사용하고, 응답에 `version_id`를 돌려줍니다.
- 미리보기(`POST /templates/{id}/preview {locale, vars, recipient}`)는 **서버에서 발송과 동일한 코드 경로**로 렌더합니다. 에디터의 즉시 미리보기만 liquidjs로 근사하고, 최종 확인은 서버 결과를 씁니다.

## 7. 캠페인 수명주기와 1M 수신자 인제스트

### 7.1 API 흐름

```
POST /campaigns                         → {id, status: draft}
POST /campaigns/{id}/recipients         (Content-Type: application/x-ndjson, 스트리밍 본문, Idempotency-Key: chunk-0007)
  {"email":"a@x.com","name":"A","locale":"ko","vars":{"plan":"pro"},"unsubscribe_url":"https://..."}
  ... 반복 호출 가능, 청크당 권장 10k~100k 줄
  → {accepted: 99871, duplicates: 129, invalid: 0, total: 1000000}
POST /campaigns/{id}/start              (schedule_at 선택) → status: scheduled|running
POST /campaigns/{id}/pause | resume | cancel
POST /campaigns/{id}/retry {status:["failed"], error_class:["transient","rate_limited"]}
GET  /campaigns/{id}                    → 상태 + 상태별 카운트 + 오픈/클릭/수신거부 유니크 수와 비율(집계 캐시, 최대 N초 지연)
POST /campaigns/{id}/unsubscribes {email | delivery_id, source:"host"}   ← unsubscribe_mode=host 일 때 호스트가 통지
POST /deliveries/{id}/unsubscribe       ← 동일, delivery 단위
GET  /campaigns/{id}/links              → 링크별 클릭 수
GET  /campaigns/{id}/deliveries?status=failed&cursor=...
```

### 7.2 인제스트 설계

- 본문을 줄 단위로 디코드해 **2,000행 배치**로 `Deliveries.InsertBatch`. 메모리는 배치 크기에 비례(상수).
- 정규화: 소문자화, 도메인 IDN→punycode, 표시명 trim. `(campaign_id, email_norm)` 유니크로 캠페인 내 중복 제거.
- **청크 멱등성**: `Idempotency-Key`(없으면 본문 해시)로 `RecipientChunk` 기록. 완료된 청크 재전송은 저장된 결과를 즉시 반환, 중간 실패 후 재전송은 유니크 인덱스 덕에 안전.
- 상태 `pending`으로 삽입. **start 시 1M 행을 갱신하지 않습니다.** 대신 sender가 "running 캠페인 집합"을 몇 초마다 갱신해 claim 조건으로 사용하므로 pause/resume은 캠페인 행 하나만 바꿉니다(ADR-0002). cancel만 백그라운드로 청크 단위 `BulkTransition`.
- 한도: 캠페인당 수신자 수, 수신자당 vars 크기(기본 8KB), 줄 길이 등을 `Limits`로 강제. 413/422로 거부.
- `start` 전제조건: 수신자 ≥1, Sender/Transport 존재, MessageVersion 고정, 누락 i18n 키 없음(정책에 따라).

### 7.3 완료 판정과 통계

control 리더 루프가 running 캠페인마다 `CountByStatus`를 주기(기본 10s)로 집계해 캠페인 행에 캐시. 미완료 상태(`pending/queued/leased/deferred`)가 0이면 `completed` + 이벤트. Delivery마다 카운터를 증가시키는 hot-row 갱신은 하지 않습니다. `total == 0`은 완료로 보지 않습니다 — 보존기간이 이미 행을 지웠거나 인제스트와 경합한 캠페인이지 "할 일이 있었고 다 끝난" 캠페인이 아닙니다. delivery 수가 `WithLargeCampaign(rows, every)`의 `rows`(기본 10만)를 넘으면 `every`틱(기본 6)에 한 번만 `CountByStatus`를 돌려, 100만 행 `COUNT`가 이 패키지에서 실제 DB를 아프게 할 수 있는 유일한 쿼리가 되지 않게 합니다.

완료 후에도 **트래킹 유니크만**은 계속 갱신합니다. 오픈·클릭·수신거부는 대부분 캠페인이 끝난 뒤에 들어오므로, 완료 시점에 쓴 값으로 굳으면 §9.3의 수신거부율이 사실상 영원히 0이 됩니다. 같은 finalizer 틱이 `completed` 캠페인을 페이지 단위로(`Batches.StatsRefreshScan`, 기본 2000건씩, 커서는 다음 틱으로 이어짐) 훑어 `CountUnique` → `UpdateStats`만 다시 씁니다 — `ByStatus`는 손대지 않습니다(끝난 캠페인의 상태 수는 확정입니다). 주기는 감쇠합니다: 완료 후 1시간은 매 틱, 그 뒤에는 10분마다(`Stats.ComputedAt` 기준), 완료 후 14일(`WithTrackingRefresh`)이 지나면 그만둡니다. 값이 안 바뀌어도 `ComputedAt`은 올립니다 — 안 올리면 다음 틱이 같은 캠페인을 다시 셉니다.

그래서 finalizer는 `ActiveTenants`가 아니라 **모든 테넌트**를 틱합니다. 완료 판정 자체는 active 테넌트에서만 일어나지만, 갱신해야 할 그 캠페인의 테넌트는 대개 이미 한가합니다. 테넌트 목록은 리더가 30초 캐시로 공유합니다(`Provider.Tenants`는 비쌀 수 있습니다).

## 8. Sender

### 8.1 루프

```
for each tenant (round-robin, per-tenant concurrency cap):
  for lane in [transactional, bulk]:                 # 별도 고루틴 풀, transactional이 항상 먼저 채워짐
    batch = Claim(lane, runningCampaigns[tenant], limit=N, lease=2m)
    for d in batch (worker pool):
      msg = render(d)                                  # 파싱 캐시, HTML escape, 링크 재작성 + 오픈 픽셀(§9), MIME, List-Unsubscribe(+Post) 헤더, X-Sendplane-ID, VERP Return-Path
      if hooks.BeforeSend(msg) == ErrSkip → result suppressed
      transport = pick(sender.transport)              # 가중치 분배, unhealthy 제외
      limiter(transport, recipientDomain).Wait()
      res = transport.Send(msg)                        # 커넥션 풀, conn당 max_msgs 후 재접속
      results.append(classify(res))
    Complete(results)                                  # 배치 커밋; 실패 시 lease 만료로 자연 복구(at-least-once)
```

- **at-least-once** 의미론입니다. `Complete` 커밋 전에 프로세스가 죽으면 같은 메일이 한 번 더 갈 수 있습니다. 이를 줄이기 위해 SMTP 250 직후 개별 `MarkSent`를 먼저(빠른 경로) 기록하고 attempt 상세는 배치로 남기는 2단계를 옵션으로 둡니다. 정확히 한 번은 SMTP에서 불가능하므로 목표로 하지 않습니다.
- 파싱 캐시 키 `(version_id, locale)`, LRU. 1M 캠페인이라도 locale 수만큼만 파싱.
- MIME 조립: `wneessen/go-mail`의 메시지 빌더 + 자체 SMTP 클라이언트 풀(go-mail의 smtp 패키지). DKIM 서명은 `emersion/go-msgauth/dkim`로 sender에서 선택 수행(릴레이가 서명하면 끔).

### 8.2 속도 제한

- Transport 단위 목표 rate(메일/초)는 **클러스터 전역 값**으로 설정하고, 각 sender 레플리카는 `Workers` heartbeat로 파악한 활성 레플리카 수로 나눠 자기 몫을 토큰 버킷에 적용합니다. 중앙 락 없이 근사치를 얻고, 레플리카 증감에 1 heartbeat 주기 내로 수렴합니다.
- 수신 도메인별 상한(예: gmail.com 20/s)은 같은 방식으로 분배.
- `rate_limited` 응답을 받으면 해당 transport(및 도메인) 버킷을 절반으로 줄이고 cooldown 후 서서히 복구(AIMD).

### 8.3 Transport 헬스

auth/TLS/연결 실패가 연속 N회면 transport `unhealthy` → 해당 transport로 라우팅 중단, 이벤트 발행, 60s 간격 probe(EHLO+AUTH)로 자동 복구. Sender에 transport가 하나뿐이면 캠페인은 자연히 정지 상태가 되며 UI에 원인이 표시됩니다.

## 9. 트래킹 · 수신거부

### 9.1 공개 엔드포인트와 토큰

control이 인증 없는 공개 라우트를 제공합니다. 테넌트별 `tracking_domain`(예: `t.example.com`)이 이 라우트로 향해야 합니다.

| 라우트 | 용도 |
|---|---|
| `GET /t/o/{token}` | 오픈 픽셀(1×1 gif, `Cache-Control: no-store`) |
| `GET /t/c/{token}?u={url}` | 클릭 기록 후 302 리다이렉트 |
| `GET /t/u/{token}` | 수신거부 클릭 기록 후 **호스트 목적지로 302** |
| `POST /t/u/{token}` | RFC 8058 원클릭 수신거부. 기록 + 호스트 통지, 200 |

- 실제 형식(`internal/tracking`, ADR-0011과의 차이는 아래 표)은 다음과 같습니다.

  ```
  token = kid "." base64url( body ‖ mac )

  body = len(tenant_id) ‖ tenant_id ‖ len(delivery_id) ‖ delivery_id ‖ kind ‖ varint(link_no)
         [ ‖ len(dest) ‖ dest ]          // kind == unsubscribe 일 때만 dest를 본문에 싣는다
  mac  = HMAC-SHA256( secret,
           "sendplane/tracking/v1\0" ‖ len(tenant_id) ‖ tenant_id
           ‖ len(delivery_id) ‖ delivery_id
           ‖ kind ‖ varint(link_no) ‖ len(dest) ‖ dest )[:16]   // 앞 16바이트(128비트)만
  ```

  **상태 없이 검증**되고, 목적지 `dest`는 kind와 무관하게 항상 MAC에 포함되어 오픈 리다이렉트가 불가능합니다(`/t/c/{token}?u=`는 서명자가 고른 `u`로만 리다이렉트). **테넌트 ID가 토큰 본문에 들어 있습니다** — 공개 라우트는 인증이 없고 서명 키는 테넌트별이라, 토큰에 테넌트가 없으면 검증이 모든 테넌트의 키를 훑어야 합니다. MAC이 테넌트 ID도 덮으므로 다른 테넌트로 바꿔치기할 수 없습니다. 수신자별 링크를 DB에 저장하지 않습니다.

  | 항목 | 최초 설계 | 실제 구현 | 이유 |
  |---|---|---|---|
  | 구분자 | `kid ‖ base64url(...)` | `kid "." base64url(...)` | kid 길이가 가변이라 구분자 없이는 되돌릴 수 없음 |
  | MAC 길이 | 전체 HMAC-SHA256(32바이트) | 앞 16바이트만 | 128비트면 위조가 무의미하고, 토큰이 모든 메일의 모든 링크에 들어가므로 길이를 아낌 |

- 서명 키는 테넌트별로 `SecretCipher`로 **암호화하지 않고** 보관합니다(`store.SigningKey`) — 토큰을 검증하는 모든 공개 경로가 키를 그대로 읽어야 하고, 그 복제본에 cipher가 없을 수도 있기 때문입니다. `kid`로 회전하며, 옛 키를 `TrackingConfig.SigningKeys`에 남겨 두는 동안만 옛 링크가 유효합니다.
- 보존기간이 지나 삭제된 delivery의 토큰은 검증은 통과하지만 기록은 버립니다.
- VERP 주소(§10)도 같은 패키지가 만들지만 MAC 컨텍스트가 다릅니다(`"sendplane/verp/v1\0"`) — 테넌트가 같은 키를 두 용도에 재사용해도 서명이 섞이지 않습니다.

### 9.2 렌더 시 삽입 (sender)

Liquid 렌더가 끝난 HTML에 대해:
1. `clicks=on`이면 `http(s)` `<a href>`를 `/t/c/{token}?u=`로 재작성. `mailto:`/`tel:`/앵커/`data-sp-track="off"` 링크와 수신거부 URL은 제외. `link_no`는 문서 내 등장 순서이며 링크 목록은 publish 시 MessageVersion에서 추출해 둡니다(`GET /campaigns/{id}/links`). Liquid 변수로 만들어진 동적 URL은 `u` 그대로 실려 통계에서는 URL 해시로 묶입니다.
2. `opens=on`이면 `</body>` 앞에 픽셀 `<img>` 삽입. text 파트는 변경 없음.
3. `unsubscribe_mode`에 따라:
   - `sendplane`(기본, `tracking_domain` 필요): `{{ unsubscribe_url }}` = `https://t.example.com/t/u/{token}`. 호스트 목적지(수신자 변수 > 테넌트 URL 템플릿 > 훅)는 토큰 서명에 포함되어 리다이렉트에 사용. 헤더 `List-Unsubscribe: <https://t.example.com/t/u/{token}>` + `List-Unsubscribe-Post: List-Unsubscribe=One-Click`.
   - `host`: 호스트 URL을 그대로 변수와 `List-Unsubscribe`에 넣음. 호스트가 처리 후 `POST /campaigns/{id}/unsubscribes`로 통지해야 통계에 잡힘. `List-Unsubscribe-Post`는 호스트가 원클릭 POST를 받는다고 선언한 경우에만 추가.
   - `none`: transactional 기본값. 헤더/변수 생략.

### 9.3 기록과 집계

- `TrackingEvent`는 control이 **메모리 버퍼 → 1초 배치 삽입**합니다(대형 캠페인 직후 오픈 폭주 대비). 크래시 시 최대 1초분 유실을 문서화합니다. Delivery의 `first_opened_at/first_clicked_at/unsubscribed_at`은 NULL일 때만 갱신하는 조건부 UPDATE로 유니크 집계의 근거가 됩니다.
- 수신거부는 두 단계입니다: `unsubscribe_clicked`(GET 리다이렉트, 스캐너 가능성)와 `unsubscribed`(원클릭 POST 또는 호스트 API 통지, 확정). **캠페인 수신거부율 = unsubscribed 유니크 / sent**, clicked는 참고 지표. Keila가 겪은 "스캐너가 수신거부 링크를 미리 열어 버리는" 문제를 이 구분과 호스트 측 확인 페이지로 흡수합니다.
- 봇 판정: 발송 후 N초(기본 3s) 이내 클릭, 알려진 스캐너 UA/ASN, 한 delivery의 모든 링크가 수 초 내 순차 클릭된 패턴이면 `suspected_bot=true`. 기본 집계에서 제외하되 원본 보존.
- 오픈은 Apple Mail Privacy Protection 등으로 과대 추정됨을 UI에 명시하고 클릭 기반 지표를 우선 노출합니다.
- 캠페인 통계 캐시(§7.3)에 `unique_opens, unique_clicks, unsubscribed, unsubscribe_clicked`와 비율(분모 `sent`)을 추가합니다. 이 네 값은 캠페인이 `completed` 된 뒤에도 finalizer가 감쇠 주기로 다시 계산합니다(§7.3) — 수신자가 메일을 여는 시점은 거의 항상 완료 후입니다.
- 이벤트: `delivery.opened|clicked`(기본 구독 off), `recipient.unsubscribed`(기본 on, 호스트가 자기 DB를 갱신하는 주 경로). `Hooks.Unsubscribed`가 있으면 원클릭 처리 중 동기 호출하고 실패 시 이벤트로 재시도합니다.

### 9.4 개인정보

IP는 기본 저장하지 않고(테넌트 설정으로 해시 저장), UA는 봇 판정 후 요약 문자열만 남깁니다. TrackingEvent는 Delivery와 같은 보존기간을 따릅니다.

## 10. 바운스 처리

- 상관관계 근거 3중화: ① VERP `Return-Path: bounce+{deliveryID}.{hmac8}@{bounce_domain}` ② 헤더 `X-Sendplane-ID: {tenant}/{deliveryID}` ③ `Message-ID: <{deliveryID}@{domain}>`. DSN에 원본 헤더가 없어도 ①로, 릴레이가 envelope sender를 덮어써도 ②③로 찾습니다. HMAC으로 위조 바운스 주입 차단.
- 파서: `message/delivery-status`(RFC 3464) → Action/Status/Diagnostic-Code, `message/feedback-report`(RFC 5965) → complaint, 그 외 휴리스틱(제목/본문 패턴, Final-Recipient) → 신뢰도 낮음으로 표시.
- 결과: `BounceEvent` 저장(raw 보존은 테넌트 설정) → delivery `bounced|complained` 전이(이미 `sent`인 경우만) → suppression 삽입(테넌트 설정) → 이벤트 발행.
- 폴러: `emersion/go-imap/v2`(IDLE 지원 시 사용, 아니면 주기 폴링), POP3는 직접 구현(`internal/mailbox/pop3.go`). 처리 후 삭제/이동 정책 설정. 메일박스 하나당 폴러 하나(스토어 lock으로 보장) → 여러 레플리카가 같은 메일을 이중 처리하지 않음.
- 메일박스는 테넌트 리소스입니다: `store.BounceMailbox`(프로토콜·호스트·자격증명·폴더·`after_process`·`enabled`) + `/api/v1/bounce-mailboxes`. 폴러는 `Provider.Tenants`(활성 여부와 무관한 전체 테넌트)를 돌며 각 테넌트의 enabled 메일박스를 읽습니다 — 바운스는 캠페인이 끝나고 한참 뒤에 옵니다.
- **폴링은 곧 자격증명 점검입니다.** 패스마다 다이얼 결과를 `BounceMailbox.Health`(§11.5)에 씁니다 — 성공하면 `ok`, 실패하면 단계(`dial`/`tls`/`auth`/`folder`)와 사유를 남기고 연속 실패를 셉니다. 바운스 메일박스를 주기적으로 들여다보는 건 이 폴러뿐이라, 여기서 못 보면 아무도 못 봅니다.
- raw 보존은 `TenantSettings.BounceRetainRaw`(기본 off), suppression 보존기간은 `SuppressionRepo.DeleteBefore`로 control의 retention 루프가 정리합니다.
- 프로바이더 webhook(SES/SendGrid 등)은 같은 `BounceEvent` 경로로 들어오는 어댑터로 후순위 추가.

**suppression에 대한 결정**: "email DB 없음" 원칙과 약간 긴장 관계에 있지만, hard bounce/complaint 주소로의 재발송은 도메인 평판을 직접 해치므로 **테넌트 설정으로 켜고 끌 수 있는 내장 suppression(기본 on)**을 둡니다. 호스트가 자체 관리하려면 끄고 `BeforeSend` 훅(또는 API 호출 전 필터링)을 쓰면 됩니다. 저장 항목은 `email_norm, reason, source_delivery_id, created_at, expires_at`뿐이며 보존기간이 있습니다(ADR-0008).

## 11. 도메인 · 발신 헬스 체크 (루프백)

DNS 레코드만 보는 검사는 "레코드가 있다"까지만 말해 줍니다. 실제 수신 측이 SPF/DKIM/DMARC를 pass로 판정하는지, TLS로 도착하는지, 스팸함으로 가는지는 **메일을 보내서 받아 봐야** 압니다. 그래서 1차 근거는 루프백 프로브이고 DNS 검사는 원인 진단용 2차 계층입니다.

### 11.1 프로브 메일박스

- `ProbeMailbox`: IMAP 계정(호스트, 포트, 인증, 폴더 매핑 inbox/spam, `authserv-id`). 테넌트별 또는 전역. **여러 개 등록 권장**(Gmail 계정, Outlook 계정, 자체 Postfix+OpenDKIM/OpenDMARC 등). 판정은 수신 측 MTA가 붙인 헤더에 의존하므로 실제 대상 프로바이더의 메일박스가 가장 정확합니다.
- 바운스 메일박스는 그 MTA가 `Authentication-Results`를 붙이는 경우에 한해 프로브 메일박스로 겸용할 수 있습니다.

### 11.2 실행 흐름 (control 리더 루프, Sender 단위)

```
for sender in tenant.senders (주기 기본 6h, transport/domain 변경 시, 수동 트리거):
  for mbox in probe mailboxes:
    enqueue Delivery(lane=probe, sender, to=mbox.address,
                     subject "[sendplane probe {run_id}]", header X-Sendplane-Probe: {run_id}/{hmac})
    # 일반 sender 경로로 발송 → 실제 transport, DKIM 서명, Return-Path, 링크 재작성까지 동일 조건
  poll mbox (IMAP SEARCH HEADER X-Sendplane-Probe → 못 찾으면 SEARCH SUBJECT "[sendplane probe {run_id}]"로 폴백, 타임아웃 15분):
    미수신 → red("not delivered") (+ 바운스 메일박스에 run_id의 DSN이 있으면 사유 첨부)
    수신   → 파싱:
      Authentication-Results (RFC 8601): authserv-id가 mbox 설정값과 일치하는 헤더만 신뢰
        spf=pass|fail|none, dkim=pass + header.d/header.s, dmarc=pass|fail + p=, arc(있으면)
      Received 체인: 첫 외부 홉의 from 절 → HELO/PTR/IP, "with ESMTPS"/TLS 토큰 → TLS 여부
      도착 폴더: inbox | spam
      지연: 발송 시각 ~ 첫 수신 Received
    프로브 메일 삭제
  DNS 정적 검사(§11.3) 병행 → ProbeRun 저장 → 요약 상태 계산 → 변화 시 sender.health_changed 이벤트
```

- 프로브 delivery는 `lane=probe`로 캠페인 통계·트래킹(오픈 픽셀·링크 재작성·수신거부)·suppression에서 제외됩니다. `X-Sendplane-Probe`는 `Delivery.Vars["probe_token"]`에서 나옵니다.
- 두 루프(트리거 5분, 회수 1분)는 control 리더에만 등록됩니다(`control.WithLoop`). 메일박스 접속은 루트가 `internal/mailbox`를 `probe.MailboxOpener`로 감싸고, 받은편지함과 스팸함에 각각 커넥션을 엽니다.
- 두 루프 모두 `control.Loop.AllTenants` 입니다. 프로브 delivery는 1~2초면 종단 상태가 되어 테넌트가 곧바로 `ActiveTenants`에서 빠지므로, active 테넌트만 도는 회수 루프는 **테넌트가 마침 다른 일을 하고 있을 때만** 판정을 끝냅니다. 같은 이유로 `retention`·`finalizer`·`outbox-sweep`도 전체 테넌트를 돕니다.
- 프로세스 단위 설정은 `Options.Probe`(`host.ProbeConfig`: `Enabled`, `HMACKey`, `Nameservers`, `Interval`, `Timeout`)입니다. 꺼져 있으면 `POST /senders/{id}/probe`는 501입니다.
- 여러 메일박스 결과의 "최악 값"이 요약 상태이고 상세는 메일박스별로 표시합니다.
- 프로브 발송이 transport 상태(§8.3)도 갱신하므로 별도 SMTP 연결 테스트 버튼은 "프로브 즉시 실행"으로 대체합니다.
- `POST /senders/{id}/probe`가 트리거할 프로브 메일박스가 하나도 없으면 루프백 자체를 건너뛰고, DNS 체커가 설정돼 있으면 **DNS 전용 run**으로 대체해 상태 사유에 "loopback 미구성"을 남깁니다(ErrNoMailbox, ADR-0012) — 아무 진단도 안 주는 것보다는 낫다는 판단입니다.

### 11.3 DNS 정적 검사 (진단 계층)

| 항목 | 검사 | 루프백 실패 시 힌트 |
|---|---|---|
| SPF | TXT 존재/구문, lookup ≤10, **프로브 Received에서 관측된 아웃바운드 IP** 포함 여부 | `spf=fail` → "IP x.x.x.x가 SPF에 없음" |
| DKIM | `{selector}._domainkey` TXT, 키 파싱, sendplane 서명 시 개인키 일치 | `dkim=fail` → 선택자/키 불일치 |
| DMARC | `_dmarc` TXT, `p=`, `rua`, alignment 모드 | `dmarc=fail` → From 도메인과 SPF/DKIM 도메인 정렬 |
| MX | return-path 도메인 MX | 바운스 미수신 |
| PTR | 관측된 IP의 PTR + 정방향 일치(FCrDNS) | Received의 rdns 불일치 |

- 아웃바운드 IP는 설정값이 없어도 프로브의 Received 헤더에서 관측되므로 별도 자기 IP 탐지가 필요 없습니다.
- 리졸버는 `miekg/dns`로 지정 네임서버에 직접 질의.

### 11.4 판정과 UI

- green(전부 pass, inbox, TLS) / yellow(dmarc p=none, spam 폴더, PTR 불일치, TLS 없음) / red(미수신, spf/dkim/dmarc fail).
- 캠페인 start 시 Sender가 red면 경고(차단 여부는 테넌트 정책).
- ProbeRun 이력으로 "언제부터 깨졌는지" 추적. 원본 헤더는 진단용 보관(보존기간 적용).
- **메일박스를 못 열었으면 판정하지 않습니다.** 프로브 메일박스 접속이 실패한 채로 타임아웃이 지난 run은
  red "미수신"이 아니라 `unknown` + `probe mailbox unreachable: {단계}`로 닫힙니다. 아무것도 관측하지
  못한 실행을 sender 탓으로 돌리면, 로테이션된 IMAP 비밀번호가 DNS 추적으로 이어집니다(ADR-0015).
  sender 요약도 그 사유를 그대로 물고 가므로 화면에서 원인이 보입니다.

### 11.5 메일박스 헬스 (자격증명 감시)

프로브·바운스 메일박스는 **같은 문제**를 공유합니다: 비밀번호가 바뀌거나 앱 비밀번호가 만료되면
조용히 죽습니다. 그래서 두 모델 모두 `MailboxHealth`(`status` ok/error/unknown, `stage`, `reason`,
`checked_at`, `last_ok_at`, `consecutive_failures`)를 갖고, 리포지터리의 `UpdateHealth`로만 쓰입니다 —
낙관적 동시성에 참여하지 않고 `version`/`updated_at`도 건드리지 않으므로, 백그라운드 점검이 운영자의
편집과 경합하지 않습니다.

쓰는 주체는 넷입니다.

| 주체 | 시점 |
|---|---|
| 바운스 폴러 | 패스마다(§10) |
| 프로브 수집 루프 | 메일박스를 열 때마다(pending run이 있을 때만) |
| `mailbox-check` 리더 루프 | `AllTenants`, 기본 15분(`probe.mailbox_check_interval`). enabled 메일박스 중 `checked_at`이 간격보다 오래된 것만, 틱당 20개까지, 오래된 순으로 |
| 테스트 엔드포인트 | 운영자가 누를 때 |

`mailbox-check`가 따로 있는 이유는 나머지 셋이 모두 **다른 일의 부산물**이기 때문입니다: 바운스 폴링은
꺼져 있을 수 있고, 프로브 수집은 6시간마다 몇 분만 메일박스를 봅니다. 그 사이에 바뀐 비밀번호는
"최근 프로브 4번이 도착하지 않음"으로, 6시간 늦게, 엉뚱한 곳(sender)을 가리키며 나타납니다.

**테스트 엔드포인트**(`sender.write`):

- `POST /api/v1/{probe,bounce}-mailboxes/test` — 본문의 자격증명으로 접속만 해 보고 아무것도 저장하지
  않습니다. 생성 폼의 "테스트" 버튼용입니다.
- `POST /api/v1/{probe,bounce}-mailboxes/{id}/test` — 저장된 행으로 접속하고 결과를 `health`에 기록합니다.
  본문에 `password`를 주면 **저장 전에** 새 비밀번호만 시험합니다(그 경우 health는 쓰지 않습니다 — 폼의
  오타가 멀쩡한 메일박스를 고장 난 것으로 만들면 안 됩니다).

둘 다 응답은 `MailboxTestResult{ok, stage, error?, latency_ms, folders{name:{exists,messages}}, server?}`이고,
**원격 실패는 200 + `ok:false`** 입니다. 로그인 거절은 요청의 답이지 요청의 실패가 아닙니다.
`stage`는 `config`(보내 보지도 못함) → `dial` → `tls` → `auth` → `folder` → `ok` 순서이고, 20초 안에 끝납니다.

전이할 때만 이벤트가 납니다: 연속 실패가 2에 도달하면 `mailbox.unhealthy`, 거기서 복구되면
`mailbox.recovered`. 1회 실패는 blip이라 알리지 않고, 계속 실패해도 한 장애당 한 번만 알립니다.

## 12. 이벤트 · 관측성

- 이벤트는 **아웃박스 패턴**: 상태 전이와 같은 스토어에 `EventOutbox` 삽입 → control 리더 루프가 `EventSink`로 배달(webhook: HMAC 서명, 재시도, 실패 시 dead-letter 조회/재전송 API). 호스트 측 Go `EventSink` 구현이면 동기 호출.
- 이벤트 타입: `delivery.sent|deferred|failed|bounced|complained|suppressed`, `campaign.started|paused|completed|cancelled`, `transport.unhealthy|recovered`, `sender.health_changed`, `mailbox.unhealthy|recovered`, `recipient.unsubscribed`, `delivery.opened|clicked`, `i18n.missing_key`.
- `mailbox.unhealthy`/`mailbox.recovered`는 프로브·바운스 메일박스 자격증명의 전이입니다(§11.5). 페이로드는 `{kind: probe|bounce, mailbox_id, name, status, stage, reason, consecutive_failures, checked_at, last_ok_at}`. 한 메일박스당 한 장애에 한 번이라 기본 집합에 들어 있습니다.
- **구독 필터**는 `TenantSettings.EventTypes`(API `event_types`)입니다. 비어 있으면 **기본 집합** = `delivery.sent`·`delivery.opened`·`delivery.clicked`를 뺀 전부, 값이 있으면 그 목록이 곧 전부입니다. 필터는 두 번 걸립니다: **enqueue 할 때**(구독하지 않은 타입은 outbox 행 자체를 만들지 않습니다)와 **dispatch 할 때**(구독을 끄면 이미 쌓인 행도 나가지 않고, 그 행은 delivered로 정리됩니다).
- `delivery.sent`/`delivery.failed`는 sender의 배치 `Complete` 이후 같은 스토어에 씁니다. 계약은 **delivery 하나당 outbox 행 하나**이고, 그래서 100만 수신자 캠페인을 구독하면 outbox 행·dispatch·HTTP POST가 100만 건입니다 — `delivery.sent`가 기본 집합에서 빠져 있는 이유가 그것입니다. 켜기 전에 그 비용을 계산하십시오.
- outbox 디스패처는 두 개입니다: active 테넌트(+3틱 유예)를 1초마다 도는 것(캠페인 전이의 지연을 짧게)과, **모든 테넌트**(`Provider.Tenants`)를 10초마다 도는 `outbox-sweep`. 바운스(며칠 뒤)·프로브 판정(1분 뒤)처럼 테넌트가 한가해진 뒤에 생기는 이벤트는 sweep이 아니면 다음 캠페인 때까지 pending으로 남습니다. `ClaimPending`이 lease를 잡으므로 둘이 같은 행을 두 번 보내지 않습니다.
- 디스패치 실패는 `1m·5m·30m·2h·12h`(마지막 값 반복) 백오프이고, **10회 실패 후 `failed`로 고정**(dead letter, `GET /events?status=failed` + `POST /events/{id}/replay`). `Hooks.Events`가 nil이면 두 디스패처 루프 자체를 등록하지 않습니다 — 아무도 소비하지 않는 행의 시도 횟수만 태우는 걸 막기 위해서입니다.
- 메트릭(OpenTelemetry/Prometheus): 큐 깊이(lane/tenant), claim 지연, 렌더/SMTP 지연 히스토그램, 상태 전이 카운터, transport 상태, rate limiter 대기. HPA/KEDA는 큐 깊이 메트릭 사용.
- 로그는 `slog`, 요청/딜리버리 ID 상관관계.

## 13. 프론트엔드

```
web/
  pnpm-workspace.yaml
  packages/api      @sendplane/api      openapi.yaml → openapi-typescript 타입 + 얇은 fetch 클라이언트. 프레임워크 무관, 인증 헤더 주입 함수만 받음
  packages/ui       @sendplane/ui       Vue 3 컴포넌트 + "페이지 컴포넌트"(TemplateEditorPage, CampaignDetailPage, DeliveriesPage, DomainHealthPage ...)
                                         - vue-router 의존 없음: `navigate(to)` 와 client 를 provide/inject 로 주입, 라우팅 결정은 호스트가
                                         - UI 문자열은 vue-i18n 메시지 번들을 외부 주입 가능(기본 en/ko 동봉)
                                         - 테마는 CSS 변수. 무거운 에디터는 dynamic import
  apps/console      @sendplane/console  Vite SPA. router + 인증 어댑터(API Key 입력/JWT 콜백) + 위 페이지 조립. 참조 바이너리에 embed
```

- 블록 편집기: **GrapesJS + grapesjs-mjml**을 `MjmlBlockEditor.vue`로 래핑. 저장 형식은 `{editor:"grapesjs-mjml", editor_version, project: <json>, mjml: "<mjml>…"}`. 서버는 `mjml`만 컴파일하고 `project`는 편집 재개용으로 보관. (listmonk v5의 email-builder-js는 React 전용이라 제외. 스파이크로 검증 필요 — roadmap Phase 8)
- 템플릿 편집 화면: 모드 탭(blocks/mjml/html), 우측 i18n 패널(키×로케일 표, 누락 강조, YAML import/export), 미리보기(로케일/샘플 vars 선택, 서버 렌더).
- 운영 화면 우선순위: 캠페인 상세(상태별 카운트, 재시도 버튼) → Deliveries(필터/검색, attempt 타임라인) → Transports 헬스 → Sender 헬스(루프백 결과) → Events(페이로드/재전송) → Suppressions.

## 14. 배포 (Helm), 콘솔 임베드

- 차트 `deploy/helm/sendplane`: `control`(Deployment, ≥2, PDB), `sender`(Deployment + HPA, CPU 기준), `bounce`(Deployment, 1), `migrate`(pre-install/upgrade Job), ConfigMap(config.yaml), Secret(DSN, 암호화 키, API key), ServiceMonitor 선택. 큐 깊이 기반 KEDA `ScaledObject`는 `templates/hpa-sender.yaml`에 **주석 처리된 예시로만** 있습니다 — 클러스터에 KEDA가 설치돼 있을 때 CPU HPA를 끄고 별도 적용하는 용도이고, 이 차트가 기본으로 만드는 실제 리소스는 아닙니다.
- DB는 차트에 포함하지 않음(외부 Postgres/Mongo). 참조 바이너리는 설정 파일로 auth/webhook을 구성하므로 Go 없이도 배포 가능.
- 이미지 하나(`sendplane` + `chaos-smtp`, `deploy/dev/Dockerfile`)를 `--roles`로 나눠 씀 → 버전 불일치 위험 제거. 같은 Dockerfile이 **웹 빌드 스테이지**(`node:22-alpine`에서 `pnpm build`)를 먼저 돌려 `web/apps/console/dist`를 만들고, Go 빌드 스테이지가 그 결과를 `cmd/sendplane/console/dist`로 복사해 `//go:embed`합니다(ADR-0010) — 이미지 빌드에 Node가 필요하지만 로컬 `go build ./cmd/sendplane`은 커밋된 플레이스홀더 `dist/index.html` 덕분에 Node 없이도 됩니다.
- 콘솔은 control 역할이 켜져 있을 때만 `config.yaml`의 `console.path`(기본 `/console`, 끌 수 있음)에 마운트됩니다. `sp.Handler()`가 이미 `/api/v1`과 `/t/`를 `/` 아래에 소유하므로 `/`가 아니라 `/console`입니다. `make console-sync`(`VITE_BASE=/console/ pnpm build` → `cmd/sendplane/console/dist`로 복사)로 로컬 빌드를 갱신합니다.

## 15. 테스트 · CI 전략

| 워크플로우 | 트리거 | 내용 |
|---|---|---|
| `ci.yml` | PR, `main` push | `lint`(gofmt, go vet, golangci-lint) · `test`(**postgres:16/mongo:7 services 매트릭스**, `go test -race -count=1 ./...`, storetest 포함) · `web`(pnpm install → `pnpm gen` 후 drift 검사 → lint → typecheck → test → build → 콘솔을 `cmd/sendplane/console/dist`에 복사해 `go build ./cmd/sendplane`로 embed까지 컴파일 확인) — 3개 독립 잡 |
| `e2e.yml` | PR, `main` push, 매일 02:40 UTC, 수동 | docker compose(control + sender×2 + bounce + chaos-smtp + postgres + GreenMail IMAP/SMTP)를 띄우고 `go run ./test/e2e --kill-sender`로 시나리오 8개(부트스트랩, 1만 건 벌크 캠페인, transactional, 트래킹/수신거부, 바운스/ARF/위조 DSN, 루프백 프로브, 웹훅 이벤트, sender SIGKILL 복구)를 실행. postgres는 PR/push마다, **mongo는 매일 02:40 UTC 스케줄에서만**(`docker-compose.mongo.yml` 오버레이) 추가. 잡 타임아웃 20분(하니스 자체 `--budget` 10분이 먼저 걸림) |
| `load-1m.yml` | 매일 03:40 UTC, 수동(`recipients`/`kill_sender` 입력) | §15.1. 잡 타임아웃 45분 |

### 15.1 1M 부하 테스트 설계

- 러너: `ubuntu-latest`. 예산 **45분** 초과 시 실패(느려짐 회귀 감지). `test/load/docker-compose.yml`(postgres + control 1 + sender 3 + chaos-smtp 1)을 `go run ./test/load`가 띄우고 운영합니다.
- `chaos-smtp` 옵션 기본값: `--seed=42 --tempfail=0.05 --permfail=0.01 --drop=0.005`.
  실패 여부는 `chaossmtp.Decide(seed, rates, rcpt, attemptNo)` = `SHA256(seed‖lower(rcpt)‖0x00‖attemptNo)`의 상위 53비트를 `[0,1)`로 정규화해 `tempfail→permfail→drop` 누적 구간에 떨구는 **순수 함수**로 **결정적**이므로, 스냅샷 없이 같은 함수를 호출해 기대 `sent`/`failed`/`attempts`를 미리 계산합니다(`test/load/expect.go`).
- 단계: NDJSON 인제스트(목표 <3분, 청크 50k, 청크 하나는 같은 키/다른 키로 두 번 재전송해 멱등성 검증) → start → 5초 간격 폴링 → 완료 후 단언 → `report.json` + Markdown 요약.
- 단언: `sent + failed + suppressed == N`, 진행 중 상태 0, `sent`/`failed`가 기대값과 **정확히 일치**, chaos-smtp `Accepted == sent`(SIGKILL 시에만 최대 0.01%까지 중복 허용), 실패 표본 200건이 전부 `attempt_count == max_attempts` 또는 permanent/policy, 인제스트 < 예산, 전체 < 45분, 캠페인에 추적 링크 ≥1.
- 산출물(`report.json`, GitHub Step Summary): 처리량(msg/s), 인제스트 행/초, 상태별 카운트, 기대값, chaos-smtp 카운터, `duplicates_from_recovery`, `created_at→sent_at` p50/p95, DB 크기, 단계별 소요 시간. 절대 수치는 러너 노이즈가 있으므로 **회귀 판정은 예산(45분)과 정합성 단언만**으로 하고 처리량은 추세 기록용.
- 테스트 중 sender 하나를 진행률 10% 지점에서 SIGKILL 후 재시작(lease 회수 경로 검증)하는 단계를 포함(`--kill-sender`, nightly 기본 on).

**실측(로컬 100k 런, 이 문서 갱신 시점에 직접 실행)**: `make load-test`(10만 건, `--kill-sender`)로 인제스트 **9,155 rows/s**(10만 행 10.9초, 예산 180초), 캠페인 완료까지 발송 구간 288.2초(평균 **346 msg/s**), `sent=98,915 failed=1,085`가 기대값과 **정확히 일치**, SIGKILL 복구로 인한 중복 발송 **0건**, 전체 소요 311.3초. 출처: `test/load/README.md`의 "로컬에서 10만 건 돌리기" 절차 그대로 이 세션에서 실행한 결과(§15.1의 표와 함께 `test/load/README.md`에 기록) — 공용 CI 러너의 nightly 100만 건 수치와는 다를 수 있습니다(§15.1의 "추세 기록용" 원칙 그대로).
`make e2e`(`--kill-sender`, postgres)는 시나리오 7개 전부 PASS, 총 소요 **3m10.6s**(주로 시나리오 2: 1만 건 벌크 캠페인 1m54.3s)였습니다. 출처: `test/e2e/README.md`에 기록한 같은 세션의 실행 결과.

## 16. 보안 · 격리 체크리스트

- 인증/인가는 호스트 책임이지만 **모든 핸들러는 `Authorize` 호출을 강제**(라우터 미들웨어에서 Action 미지정 라우트는 컴파일 타임 테이블로 검출).
- 테넌트 격리: `Store`가 테넌트 바인딩이라 누락 불가. storetest에 교차 테넌트 접근 테스트 포함.
- 템플릿: Liquid 샌드박스(파일/include 없음), 렌더 타임아웃, 출력 크기 상한, HTML 기본 이스케이프.
- 헤더 인젝션: 이메일/표시명/subject의 CR/LF 거부, 커스텀 헤더 화이트리스트.
- SSRF: webhook URL은 사설 대역 차단(테넌트 설정으로 허용 목록).
- 비밀: Transport 비밀번호·DKIM 키·IMAP 비밀번호는 `SecretCipher`로 암호화 저장, API 응답에서 마스킹.
- 바운스 위조: VERP HMAC 검증 실패는 `unverified`로 기록만.
- 트래킹 URL: 목적지가 서명에 포함되어 오픈 리다이렉트 불가, 토큰 위조 불가. 공개 라우트는 rate limit 적용, 픽셀/리다이렉트 외 응답 없음.
- 개인정보: Delivery/BounceEvent/Suppression 보존기간(테넌트별), 캠페인 삭제 시 수신자 데이터 함께 삭제, raw 바운스 저장은 opt-in.

## 17. 주요 기술 선택 요약

| 영역 | 선택(go.mod / web/package.json 기준) | 대안/비고 |
|---|---|---|
| Go | 1.25.0 | |
| HTTP | `net/http` + `go-chi/chi/v5` v5.3.2 (경량, 호스트 mux에 마운트 용이) | echo/gin은 호스트와 충돌 가능성 |
| OpenAPI | `oapi-codegen/v2` v2.8.0(서버 타입, `tool` 디렉티브로 고정) + `openapi-typescript` v7.13.0(클라이언트) | 스펙 우선 |
| Postgres | `jackc/pgx/v5` v5.11.0, 마이그레이션은 `migrations/*.sql`을 `embed`로 넣은 **자체 구현**(advisory lock으로 동시 기동 안전) | `golang-migrate` 등 외부 마이그레이션 라이브러리는 쓰지 않음 |
| Mongo | 공식 `go.mongodb.org/mongo-driver/v2` v2.9.1 | 트랜잭션 미사용(standalone 호환, ADR-0007) |
| 인증 | `golang-jwt/jwt/v5` v5.3.1 + `MicahParks/keyfunc/v3` v3.8.2(JWKS) | 참조 바이너리의 JWT 모드 |
| Liquid | `osteele/liquid` v1.9.2 | 단일 중괄호 커스텀 문법은 ADR-0004에서 기각, 사용자 확인 완료 |
| MJML | `Boostport/mjml-go` v0.16.0(WASM/wazero, Node 불필요, 자체 스레드 안전) | 컴파일은 publish 시에만(ADR-0009) |
| HTML→text | 자체 구현(`internal/render/html2text.go`, `x/net/html` 토크나이저) | `jaytaylor/html2text`(foster parenting이 `{% if %}`를 이동시킴), `k3a/html2text`(정규식이라 MJML 산출물이 한 줄로 뭉개짐) 둘 다 기각(§6.3) |
| SMTP | `wneessen/go-mail` v0.8.1 (메시지 빌더 + smtp 패키지 기반 자체 풀) | |
| DKIM | `emersion/go-msgauth` v0.7.0(`dkim`) | 릴레이 서명 시 비활성 |
| IMAP | `emersion/go-imap/v2` v2.0.0-beta.8 (+ `imapclient`, IDLE/UIDPLUS/MOVE) | |
| POP3 | **자체 구현**(`internal/mailbox/pop3.go`, RFC 1939 + STLS) | `knadh/go-pop3`는 STARTTLS·context·커스텀 TLS 설정이 없어 기각 |
| DNS | `miekg/dns` v1.1.73, 지정 네임서버 직접 질의 | 루프백 프로브의 진단 계층, SPF `check_host()` 부분 구현(매크로 미확장) |
| 트래킹 토큰 | HMAC-SHA256(앞 16바이트만), `kid.`+base64url, 테넌트 ID를 페이로드에 포함 | 상태 없는 검증(§9.1) |
| ID | UUIDv7 (`google/uuid`) | 시간 정렬, 양 DB 호환 |
| YAML | `gopkg.in/yaml.v3` | i18n 번들 export의 결정적 순서 보장 |
| 프론트 | Vue 3.5 + Vite 7 + TS 5.9, pnpm 10 workspace, vue-i18n 11, vue-router 4(콘솔만), GrapesJS 0.23 + grapesjs-mjml 1.0(에디터, 동적 import) | Node ≥22 |
| 관측 | OpenTelemetry(`Metrics` 인터페이스로 주입), slog | Prometheus exporter는 호스트가 `Metrics` 구현체로 제공(엔진 자체는 특정 exporter에 묶이지 않음) |

## 18. 열어 둔 것 (2026-09-21 기준, 실제로 남아 있는 것)

Liquid 문법(ADR-0004)과 내장 suppression 기본 on(ADR-0008)은 사용자 확인을 거쳐 **결정됐습니다** — 더 이상 열린 항목이 아닙니다. GrapesJS-MJML 블록 에디터도 스파이크 후 ADR-0009로 확정되어 구현되어 있습니다. 아래는 구현을 끝낸 뒤에도 실제로 남아 있는 것들입니다.

1. **`probe.Trigger`는 첫 메일박스의 run id만 돌려줍니다.** 한 트리거가 만든 run들은 `GroupID`를 공유하지만 `ProbeRunRepo.ListByGroup`이 없어 `POST /senders/{id}/probe`가 그룹 전체를 한 번에 조회해 주지 못합니다(`internal/probe/README.md`).
2. **`SendingDomain.OutboundIPs`는 프로브가 갱신하지 않습니다.** 관측 아웃바운드 IP는 `ProbeRun.ObservedIP`에만 남고, 도메인 행까지 쓰려면 낙관적 갱신이 하나 더 필요합니다 — 그 소유권을 control에 둘지는 아직 미정입니다.
3. **Helm 차트의 KEDA는 예시 코드일 뿐 실제 리소스가 아닙니다.** `templates/hpa-sender.yaml`의 큐 깊이 기반 `ScaledObject`는 주석 처리돼 있고, 이 차트가 기본으로 만드는 것은 CPU HPA뿐입니다. 큐 깊이 기반 오토스케일을 쓰려면 KEDA를 따로 설치하고 그 예시를 직접 적용해야 합니다.
4. **mongo는 CI에서 nightly에만 돕니다** (`ci.yml`의 storetest 매트릭스는 매 PR, `e2e.yml`의 mongo 오버레이는 02:40 UTC 스케줄에서만). PR 단위로는 mongo 경로의 e2e 회귀를 못 잡습니다.
5. **Helm 차트는 실제 k8s 클러스터에 배포해 본 적이 없습니다.** `helm template`/`helm lint` 수준 검증만 가능하고, HPA·PDB·ServiceMonitor가 실 클러스터에서 기대대로 동작하는지는 확인되지 않았습니다.
6. **GitHub Actions 실행 이력이 없습니다.** `git remote`는 `github.com/sendplane/sendplane`를 가리키고 저장소 자체는 존재하지만(`gh repo view` 확인), 이 문서 갱신 시점까지 `gh run list`에 워크플로 실행이 한 건도 없습니다 — `ci.yml`/`e2e.yml`/`load-1m.yml`이 실제 GitHub 러너에서 통과하는지는 로컬 실행으로만 확인된 상태입니다.
7. **store 계약에 없어서 못 하는 것들** (`internal/control/README.md`, `internal/bounce/README.md`): `TrackingEvent`에 유니크 여부를 남기는 필드가 없어 유니크 판정은 `CountUnique` 재계산에만 의존; 캠페인이 없는(transactional) delivery의 보존기간 삭제 경로가 없음; "pending 이벤트가 있는 테넌트"·"최근 완료된 캠페인"을 직접 조회할 방법이 없어 `outbox-sweep`과 finalizer의 커서 훑기로 대신함; `BounceType`에 auto-reply 값이, `BounceSource`에 "상관관계 실패" 값이 없어 각각 플래그와 `heuristic`으로 대신 표시.
8. **낙관적 동시성의 `version`이 응답에서 읽기 전용**이라 편집 화면은 항상 "읽은 객체"를 들고 있어야 업데이트를 보낼 수 있습니다(`web/README.md` "아직 남은 것").
9. **ICU 복수형**, 테넌트 전역 공유 i18n 번들: 키 컨벤션(`count.one`/`count.other`)으로 시작, 전용 지원은 없음.
10. **큐 백엔드 교체(NATS 등)**: `DeliveryRepo.Claim/Complete`가 경계로 설계돼 있지만 현재는 DB-as-queue만 구현.
11. **Postgres 파티셔닝/RLS**: 운영 데이터가 근거를 주면 도입.
12. **첨부파일**: 아직 없음. transactional 소형 첨부(base64, 총 10MB)만 후보, 캠페인 첨부는 비권장.
13. **프로브 메일박스 프로바이더 특이점**: Gmail/Outlook의 스팸 폴더 IMAP 이름과 `authserv-id` 값은 테스트 픽스처로만 관리되고, 새 프로바이더를 추가할 때마다 수동 검증이 필요.
