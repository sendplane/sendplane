# sendplane 아키텍처 설계

> 상태: 초안 v2 (2026-09-20, 트래킹·수신거부·루프백 헬스체크 반영). 결정 근거는 [adr/](adr/) 참조, 구현 순서는 [roadmap.md](roadmap.md) 참조.

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
├── sendplane.go            // New(), Options, Hooks, Principal, Action 상수  ← 공개 API
├── store/                  // 리포지터리 인터페이스 + 모델 (공개: 커스텀 DB 구현용)
│   ├── postgres/           // pgx 기반 구현 + migrations/
│   ├── mongo/              // mongo-driver 기반 구현 + index bootstrap
│   └── storetest/          // 적합성 스위트: storetest.Run(t, newStore)
├── cmd/
│   ├── sendplane/          // 참조 바이너리 (--roles, 설정 파일 기반 auth/hook 구현, UI embed)
│   └── chaos-smtp/         // 테스트용 실패 주입 SMTP 서버
├── internal/
│   ├── api/                // HTTP 핸들러, OpenAPI(api/openapi.yaml)와 1:1
│   ├── control/            // 리더 루프들(scheduler, finalizer, dnscheck, retention, outbox)
│   ├── ingest/             // NDJSON 수신자 스트리밍 인제스트
│   ├── render/             // Liquid 엔진, i18n 태그, MJML 컴파일, text 자동생성
│   ├── sender/             // claim loop, transport pool, rate limiter, retry policy, error classifier
│   ├── bounce/             // IMAP/POP3 poller, DSN(RFC3464)/ARF(RFC5965) 파서, VERP
│   ├── dnscheck/           // SPF/DKIM/DMARC/MX/PTR 체커
│   └── events/             // 아웃박스 → EventSink 디스패치
├── api/openapi.yaml        // 단일 진실 원천. Go 서버 타입과 TS 클라이언트를 여기서 생성
├── web/                    // pnpm workspace (§13)
├── deploy/helm/sendplane/
└── docs/
```

공개 패키지는 `sendplane`(루트)과 `store`뿐입니다. 나머지는 `internal/`로 막아 Hyrum's Law 표면을 최소화합니다.

## 3. 임베딩 API (`sendplane.New`)

```go
package sendplane

type Options struct {
    Store   store.Provider   // 필수
    Auth    Authenticator    // 필수. 요청 → Principal
    Authz   Authorizer       // 선택. 기본: 인증되면 전부 허용
    Tenants TenantResolver   // 선택. 기본: Principal.TenantID, 없으면 "default"
    Hooks   Hooks
    Secrets SecretCipher     // SMTP 비밀번호 등 at-rest 암호화. 기본: AES-GCM(env key)
    Limits  Limits           // 캠페인당 최대 수신자, 본문 크기, vars 크기 등
    Logger  *slog.Logger
    Meter   metric.MeterProvider // OpenTelemetry
    Clock   func() time.Time
}

type Principal struct {
    ID       string
    TenantID string            // TenantResolver가 없을 때 사용
    Roles    []string          // 호스트 정의 문자열, sendplane은 해석하지 않음
    Attrs    map[string]any
}

type Authenticator interface {
    Authenticate(r *http.Request) (*Principal, error) // 실패 시 ErrUnauthenticated
}

// Action은 sendplane이 정의하는 폐쇄 집합. 호스트는 이걸 자기 RBAC에 매핑한다.
type Action string
const (
    ActionTemplateRead  Action = "template.read"
    ActionTemplateWrite Action = "template.write"
    ActionCampaignRead  Action = "campaign.read"
    ActionCampaignWrite Action = "campaign.write"
    ActionCampaignSend  Action = "campaign.send"      // 관리와 발송 분리 (listmonk 교훈)
    ActionMessageSend   Action = "message.send"       // transactional
    ActionSenderWrite   Action = "sender.write"       // SMTP/도메인 설정
    ActionDeliveryRead  Action = "delivery.read"
    ActionEventRead     Action = "event.read"
    // ...
)
type Resource struct{ Kind, ID, TenantID string }
type Authorizer interface {
    Authorize(ctx context.Context, p *Principal, a Action, r Resource) error // ErrForbidden
}

type TenantResolver interface {
    Resolve(ctx context.Context, r *http.Request, p *Principal) (tenantID string, err error)
}

type Hooks struct {
    // 수신거부 "목적지"(호스트 URL). 우선순위: 수신자 unsubscribe_url 변수 > 테넌트 URL 템플릿 > 이 훅.
    // unsubscribe_mode=sendplane 이면 메일에는 sendplane 트래킹 URL이 들어가고 클릭 시 이 목적지로 리다이렉트된다(§9).
    UnsubscribeURL func(ctx context.Context, rc RecipientContext) (string, error)
    // 원클릭(List-Unsubscribe-Post) 수신거부가 sendplane 엔드포인트로 들어왔을 때 호스트에 동기 통지. 없으면 이벤트(webhook)로만 전달.
    Unsubscribed func(ctx context.Context, u UnsubscribeNotice) error
    // 발송 직전 거부/수정. 호스트 측 suppression, 법적 차단 등. ErrSkip 반환 시 status=suppressed.
    BeforeSend func(ctx context.Context, m *OutboundMessage) error
    // delivery.sent/failed/bounced/complained, campaign.completed, domain.health_changed ...
    Events EventSink // 기본: 아웃박스 → 테넌트 설정의 webhook URL
}

func New(o Options) (*Sendplane, error)
func (s *Sendplane) Handler() http.Handler                 // /api/v1 라우터. 호스트 mux에 mount
func (s *Sendplane) RunControl(ctx context.Context) error  // 리더 루프
func (s *Sendplane) RunSender(ctx context.Context, c SenderConfig) error
func (s *Sendplane) RunBounce(ctx context.Context) error
```

- **Go 코드 없이도 통합 가능**해야 합니다(비-Go 호스트). 그래서 수신거부 URL은 "수신자 변수" 또는
  "테넌트 URL 템플릿(Liquid, 예: `https://app.example.com/u?e={{ recipient.email | url_encode }}&t={{ recipient.vars.unsub_token }}`)"
  으로도 줄 수 있고, 이벤트는 webhook으로 받을 수 있습니다. Go 훅은 escape hatch입니다.
- 참조 바이너리(`cmd/sendplane`)는 `Authenticator`로 **정적 API Key(테넌트 매핑 포함) / JWT(JWKS)** 두 구현을 설정 파일로 제공합니다.

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
package store

type Provider interface {
    ForTenant(ctx context.Context, tenantID string) (Store, error)
    ActiveTenants(ctx context.Context) ([]string, error) // sender 폴링 대상. shared 모드: DISTINCT tenant_id WHERE 작업 존재
    Migrate(ctx context.Context) error
    Close() error
}

type Store interface {
    Tenant()      TenantSettingsRepo
    Transports()  TransportRepo
    Senders()     SenderRepo
    Domains()     DomainRepo         // + DomainCheckRepo
    Layouts()     LayoutRepo
    Templates()   TemplateRepo       // + i18n bundle
    Versions()    MessageVersionRepo
    Campaigns()   CampaignRepo       // + RecipientChunkRepo
    Deliveries()  DeliveryRepo
    Attempts()    AttemptRepo
    Suppressions() SuppressionRepo
    Bounces()     BounceRepo
    Outbox()      OutboxRepo
    Locks()       LockRepo           // 리더 선출 / 싱글턴 잡 lease
    Workers()     WorkerRepo         // sender heartbeat (rate 분배용)
}

// 큐 역할을 하는 핵심 메서드
type DeliveryRepo interface {
    InsertBatch(ctx, []Delivery) (inserted int, err error)        // (campaign_id, email_norm) 유니크로 멱등
    Claim(ctx, ClaimRequest) ([]Delivery, error)
    Complete(ctx, []DeliveryResult) error                          // 상태 전이 + attempt insert, 배치
    ReleaseExpiredLeases(ctx, now time.Time, limit int) (int, error)
    CountByStatus(ctx, campaignID) (map[Status]int64, error)
    BulkTransition(ctx, campaignID, from []Status, to Status, limit int) (int, error) // cancel 등, 청크 반복
    ...
}
type ClaimRequest struct {
    Lane        Lane      // transactional | bulk
    CampaignIDs []ID      // bulk: running 캠페인 집합 (nil = 제한 없음)
    Limit       int
    LeaseFor    time.Duration
    WorkerID    string
}
```

설계 규칙:
- **다중 리포지터리 트랜잭션을 요구하지 않음.** 원자성이 필요한 곳은 조건부 갱신(CAS)과 멱등 삽입으로 해결한다(Mongo, 커스텀 DB 친화).
- 모든 메서드는 `Store`가 이미 테넌트에 바인딩된 상태이므로 tenantID 인자를 받지 않는다. shared 구현이 내부적으로 `tenant_id` 조건을 강제 → 테넌트 누락 버그를 구조적으로 차단.
- `storetest.Run(t, factory)`가 계약이다: 멱등 삽입, 동시 claim 시 중복 없음, lease 만료 회수, 상태 전이 CAS, 테넌트 격리(다른 테넌트 Store로는 `ErrNotFound`) 등을 두 구현에 동일하게 실행한다.

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
- 요청서의 `{user.name}` 단일 중괄호 대신 Liquid 이중 중괄호를 채택한 이유는 ADR-0004 참조. 이 결정은 확인이 필요합니다.

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

control 리더 루프가 running 캠페인마다 `CountByStatus`를 주기(기본 10s, 캠페인 크기에 따라 증가)로 집계해 캠페인 행에 캐시. 미완료 상태(`pending/queued/leased/deferred`)가 0이면 `completed` + 이벤트. Delivery마다 카운터를 증가시키는 hot-row 갱신은 하지 않습니다.

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

- `token = kid ‖ base64url(delivery_id ‖ kind ‖ link_no ‖ HMAC(tenant_key, delivery_id‖kind‖link_no‖u))`. **상태 없이 검증**되고, 목적지 `u`가 서명에 포함되어 오픈 리다이렉트가 불가능합니다. 수신자별 링크를 DB에 저장하지 않습니다.
- 서명 키는 테넌트별로 `SecretCipher`로 보관하고 `kid`로 회전합니다.
- 보존기간이 지나 삭제된 delivery의 토큰은 검증은 통과하지만 기록은 버립니다.

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
- 캠페인 통계 캐시(§7.3)에 `unique_opens, unique_clicks, unsubscribed, unsubscribe_clicked`와 비율(분모 `sent`)을 추가합니다.
- 이벤트: `delivery.opened|clicked`(기본 구독 off), `recipient.unsubscribed`(기본 on, 호스트가 자기 DB를 갱신하는 주 경로). `Hooks.Unsubscribed`가 있으면 원클릭 처리 중 동기 호출하고 실패 시 이벤트로 재시도합니다.

### 9.4 개인정보

IP는 기본 저장하지 않고(테넌트 설정으로 해시 저장), UA는 봇 판정 후 요약 문자열만 남깁니다. TrackingEvent는 Delivery와 같은 보존기간을 따릅니다.

## 10. 바운스 처리

- 상관관계 근거 3중화: ① VERP `Return-Path: bounce+{deliveryID}.{hmac8}@{bounce_domain}` ② 헤더 `X-Sendplane-ID: {tenant}/{deliveryID}` ③ `Message-ID: <{deliveryID}@{domain}>`. DSN에 원본 헤더가 없어도 ①로, 릴레이가 envelope sender를 덮어써도 ②③로 찾습니다. HMAC으로 위조 바운스 주입 차단.
- 파서: `message/delivery-status`(RFC 3464) → Action/Status/Diagnostic-Code, `message/feedback-report`(RFC 5965) → complaint, 그 외 휴리스틱(제목/본문 패턴, Final-Recipient) → 신뢰도 낮음으로 표시.
- 결과: `BounceEvent` 저장(raw 보존은 테넌트 설정) → delivery `bounced|complained` 전이(이미 `sent`인 경우만) → suppression 삽입(테넌트 설정) → 이벤트 발행.
- 폴러: `emersion/go-imap/v2`(IDLE 지원 시 사용, 아니면 주기 폴링), POP3는 `knadh/go-pop3` 계열. 처리 후 삭제/이동 정책 설정. 메일박스 하나당 폴러 하나(스토어 lock으로 보장) → 여러 레플리카가 같은 메일을 이중 처리하지 않음.
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
  poll mbox (IMAP SEARCH HEADER X-Sendplane-Probe, 타임아웃 15분):
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

- 프로브 delivery는 `lane=probe`로 캠페인 통계·트래킹·suppression에서 제외됩니다.
- 여러 메일박스 결과의 "최악 값"이 요약 상태이고 상세는 메일박스별로 표시합니다.
- 프로브 발송이 transport 상태(§8.3)도 갱신하므로 별도 SMTP 연결 테스트 버튼은 "프로브 즉시 실행"으로 대체합니다.

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

## 12. 이벤트 · 관측성

- 이벤트는 **아웃박스 패턴**: 상태 전이와 같은 스토어에 `EventOutbox` 삽입 → control 리더 루프가 `EventSink`로 배달(webhook: HMAC 서명, 재시도, 실패 시 dead-letter 조회/재전송 API). 호스트 측 Go `EventSink` 구현이면 동기 호출.
- 이벤트 타입: `delivery.sent|deferred|failed|bounced|complained|suppressed`, `campaign.started|paused|completed|cancelled`, `transport.unhealthy|recovered`, `sender.health_changed`, `recipient.unsubscribed`, `delivery.opened|clicked`, `i18n.missing_key`.
  대량 캠페인에서 delivery 단위 이벤트는 폭주하므로 테넌트별 구독 필터(기본: 실패류만)와 배치 페이로드를 지원.
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

## 14. 배포 (Helm)

- 차트 `deploy/helm/sendplane`: `control`(Deployment, ≥2, PDB), `sender`(Deployment + HPA/KEDA, 큐 깊이 기반), `bounce`(Deployment, 1), `migrate`(pre-install/upgrade Job), ConfigMap(config.yaml), Secret(DSN, 암호화 키, API key), ServiceMonitor 선택.
- DB는 차트에 포함하지 않음(외부 Postgres/Mongo). 참조 바이너리는 설정 파일로 auth/webhook을 구성하므로 Go 없이도 배포 가능.
- 이미지 하나(`sendplane`)를 `--roles`로 나눠 씀 → 버전 불일치 위험 제거.

## 15. 테스트 · CI 전략

| 워크플로우 | 트리거 | 내용 |
|---|---|---|
| `ci.yml` | PR/push | go vet/lint, 단위 테스트, **`storetest` 매트릭스(postgres:16, mongo:7 services)**, 프론트 lint/typecheck/unit, OpenAPI ↔ 생성물 drift 검사 |
| `e2e.yml` | PR | docker compose: control+sender+bounce+chaos-smtp+pg+테스트 IMAP. 10k 캠페인 + transactional + 바운스 주입(chaos-smtp가 DSN을 IMAP에 넣음) + 트래킹(픽셀/클릭/원클릭 수신거부/호스트 통지 API → 캠페인 비율 검증) + 루프백 프로브(chaos-smtp가 `Authentication-Results`를 붙여 IMAP에 배달, pass/fail/spam 시나리오) → 상태/이벤트 검증. mongo는 nightly |
| `load-1m.yml` | 수동 + nightly | 아래 |

### 14.1 1M 부하 테스트 설계

- 러너: public repo `ubuntu-latest` 4 vCPU / 16 GB. 예산 **45분** 초과 시 실패(느려짐 회귀 감지).
- 구성: postgres(service, 튜닝된 shared_buffers), control 1, sender 3(각 concurrency 64), `chaos-smtp` 1(인프로세스 Go SMTP 서버).
- `chaos-smtp` 옵션: `--tempfail=0.05 --permfail=0.01 --drop=0.005 --ratelimit-burst=... --latency=2ms --seed=N`.
  실패 여부는 `hash(seed, recipient, attempt_no)`로 **결정적**이므로 기대 결과(최종 sent/failed 수, 총 attempt 수)를 사전에 계산할 수 있습니다.
- 단계: 1M NDJSON 인제스트(목표 <3분, 청크 50k×20, 청크 하나는 일부러 재전송해 멱등성 검증) → start → 폴링 → 종료.
- 단언: `sent + failed + suppressed == 1,000,000`, 진행 중 상태 0, 기대 sent/failed ±0(결정적), 모든 failed의 `attempt_count == max_attempts` 또는 permanent, `DeliveryAttempt` 총합 == chaos-smtp가 받은 세션 수(중복 발송 = at-least-once 재시도 외에는 0), lease 만료로 복구된 건수 리포트.
- 산출물: 처리량(msg/s), p95 claim→sent 지연, DB 크기, 위 카운트를 JSON으로 아티팩트 업로드 + PR 코멘트. 절대 수치는 러너 노이즈가 있으므로 **회귀 판정은 예산(45분)과 정합성 단언만**으로 하고 처리량은 추세 기록용.
- 테스트 중 sender 하나를 중간에 SIGKILL 후 재시작(lease 회수 경로 검증)하는 단계를 포함.

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

| 영역 | 선택 | 대안/비고 |
|---|---|---|
| HTTP | `net/http` + `chi` (경량, 호스트 mux에 마운트 용이) | echo/gin은 호스트와 충돌 가능성 |
| OpenAPI | `oapi-codegen`(서버 타입) + `openapi-typescript`(클라이언트) | 스펙 우선 |
| Postgres | `pgx/v5`, 마이그레이션 `golang-migrate` 임베드 | |
| Mongo | 공식 `mongo-driver/v2` | |
| Liquid | `osteele/liquid` | 단일 중괄호 커스텀 문법(ADR-0004에서 기각) |
| MJML | `Boostport/mjml-go`(WASM, Node 불필요) | 컴파일은 publish 시에만 |
| SMTP | `wneessen/go-mail` (메시지 빌더 + smtp 패키지 기반 자체 풀) | |
| DKIM | `emersion/go-msgauth/dkim` | 릴레이 서명 시 비활성 |
| IMAP/POP3 | `emersion/go-imap/v2`, `knadh/go-pop3` | |
| DNS | `miekg/dns` | 루프백 프로브의 진단 계층 |
| 트래킹 토큰 | HMAC-SHA256, base64url, key id 포함 | 상태 없는 검증 |
| ID | UUIDv7 | 시간 정렬, 양 DB 호환 |
| 프론트 | Vue 3 + Vite + TS, pnpm workspace, vue-i18n, GrapesJS-MJML | |
| 관측 | OpenTelemetry, Prometheus exporter, slog | |

## 18. 열어 둔 것 (의도적 미결)

1. **템플릿 문법 확정**: Liquid(`{{ }}`) 채택 가정. 단일 중괄호 요구가 강하면 파서 교체 비용은 render 패키지에 국한됨.
2. **suppression 내장 여부**: 기본 on으로 가정. 완전 제거 요구 시 `BeforeSend` 훅만 남김.
3. **트래킹 이벤트 유실 허용치**: 1초 버퍼 배치 삽입(크래시 시 유실)으로 시작. 정확성 요구 시 동기 삽입 옵션.
4. **ICU 복수형**, 테넌트 전역 공유 번들: 키 컨벤션으로 시작.
5. **큐 백엔드 교체(NATS 등)**: `DeliveryRepo.Claim/Complete`가 경계. 현재는 DB-as-queue만 구현.
6. **Postgres 파티셔닝/RLS**: 운영 데이터가 근거를 주면 도입.
7. **첨부파일**: transactional 소형 첨부(base64, 총 10MB)만 Phase 2 후보. 캠페인 첨부는 비권장.
8. **블록 편집기 최종 선택**: GrapesJS-MJML 스파이크 결과에 따라 확정.
9. **프로브 메일박스 프로바이더 특이점**: Gmail/Outlook의 스팸 폴더 IMAP 이름과 `authserv-id` 값은 픽스처로 관리.
