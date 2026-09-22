# ADR-0017: 플랫폼 자원은 설정에서 코드로 해석, 테넌트 속성은 요청 변수

- 상태: accepted
- 날짜: 2026-09-22
- 관련: [ADR-0006](0006-multitenancy-provider.md) (멀티테넌시 Provider), [ADR-0002](0002-store-as-queue.md) (스토어가 곧 큐),
  [ADR-0008](0008-bounce-and-suppression.md) (바운스 상관관계와 suppression), [ADR-0012](0012-loopback-health-probe.md) (루프백 프로브),
  [architecture.md §3 · §5.4 · §8.2 · §10 · §11](../architecture.md),
  `store/platform.go`, `store/overlay.go`, `internal/platform/`, `host/platform.go`

## 맥락

sendplane을 SaaS로 운영하면 요구가 하나 더 생깁니다. **운영자가 자기 인프라를 테넌트에게 빌려주는 것**입니다.
릴레이 계정 하나, 워밍업된 도메인 하나, `no-reply@` 발신 신원 하나를 운영자가 갖고 있고,
자기 릴레이를 들고 오지 않은 테넌트는 그걸 씁니다.

지금까지 모든 설정은 테넌트 행이었습니다(`transport`, `sender`, `sending_domain`, `probe_mailbox`, `bounce_mailbox`).
그래서 "공유 자원"을 만드는 가장 짧은 길은 **모든 테넌트에 같은 행을 복제**하는 것입니다.
실제로 그 설계를 먼저 검토했고, 기각했습니다(아래).

동시에 두 번째 요구가 붙습니다. 공유 발신 신원은 테넌트마다 **주소가 달라야** 합니다.
`sender+acme@mail.example.com`, `sender+globex@mail.example.com` — 그래야 바운스와 평판이 테넌트 단위로 분리되고,
받는 사람이 누가 보냈는지 압니다. 그 주소를 만들려면 테넌트의 **slug**를, `From` 표시 이름을 만들려면 **이름**을 알아야 합니다.
sendplane에는 그 정보가 없습니다. 그리고 **없어야 합니다**: ADR-0006이 이미 "sendplane은 테넌트 생명주기를 관리하지 않는다"로
정해 두었고, 테넌트의 이름과 slug는 호스트 애플리케이션의 DB에 있습니다.

## 결정

### 1. 플랫폼 자원은 설정이고, 스토어에 기록하지 않는다

공유 transport / sending domain / sender / probe mailbox / bounce mailbox는 **설정**(`sendplane.Options.Platform`,
참조 바이너리의 `platform:` 섹션)이고, **읽을 때마다 코드에서 가상 엔티티로 해석**합니다.
가상 ID는 `sys:<이름>` 입니다 (`sys:default`, `sys:shared-eu`). UUID에는 콜론이 들어갈 수 없으므로 두 ID 공간은 절대 충돌하지 않습니다.

`store.WithPlatform(provider, catalog, cipher, now)` 가 `Provider` 를 감싸서 이 일을 합니다.

- **읽기**(`Get`/`List`/`ListEnabled`)에 설정에서 만든 항목이 `Shared: true` 로 섞여 나옵니다.
  비밀번호와 DKIM 키는 감싸는 시점에 호스트의 `SecretCipher` 로 **메모리에서 암호화**해 둡니다.
  그래야 `internal/sender` · `internal/bounce` · `internal/probe` 의 기존 복호화 경로가 **하나도 바뀌지 않습니다**.
- **쓰기**는 `store.ErrReadOnly` (API에서 `403 platform_read_only`)입니다.
  단 하나의 예외가 **런타임 상태**입니다: transport의 서킷 상태, sender/domain의 프로브 판정, 메일박스 도달성.
  이건 `_system` 테넌트의 **shadow 행**으로 갑니다.
- **shadow 행**은 가상 ID와 `Shared: true` 와 상태 컬럼만 들고, **모든 설정 컬럼이 빈 값**입니다.
  host도, port도, username도, password도, DKIM 키도 없습니다. 읽을 때 설정을 shadow 위에 덮습니다.

즉 **공유 릴레이의 비밀번호는 어느 데이터베이스에도 존재하지 않습니다.** 결과가 셋입니다.

1. 비밀번호 교체가 마이그레이션이 아니라 **배포**입니다.
2. 한 테넌트의 DB 덤프에 다른 테넌트가 쓰는 릴레이 자격증명이 **들어갈 수 없습니다**.
3. 설정 파일과 DB가 **불일치할 수 없습니다** — DB에는 사본이 없으니까.

이 불변식은 주장이 아니라 검사 대상입니다. `store/platformtest` 가 모든 백엔드에 대해
"shadow 행에 설정 값이 하나라도 있으면 실패"를 확인하고, e2e 시나리오 9가 실제 Postgres에서 같은 것을 SELECT로 확인합니다.

### 2. 테넌트 레지스트리는 만들지 않는다. 테넌트 속성은 요청 변수다

`tenant_vars` 가 캠페인 / 트랜잭션 메시지 / 프리뷰 요청에 실려 오고, 호스트의 `Hooks.TenantVars` 훅이 검증·치환합니다.
훅이 돌려준 것이 저장되고(`Campaign.TenantVars`, `Delivery.TenantVars`) 모든 템플릿에서 `tenant` 로 바인딩됩니다.

공유 sender의 `from_name` / `from_email` / `reply_to` 는 그 위의 **Liquid 템플릿**입니다.

```yaml
from_email: "sender+{{ tenant.slug }}@mail.example.com"
from_name:  "{{ tenant.name }}"
```

요청이 들어온 시점에 렌더해 보고, 템플릿이 읽는 변수가 없으면 **`422 tenant_vars_missing`** 으로 키를 나열합니다.
`sender+@mail.example.com` 으로 메일이 나가는 것보다 낫습니다. 필요한 키는 시작 시점에 `render.ExtractVars` 로 추출합니다.

훅의 전형적인 구현은 **요청이 보낸 값을 무시하고 자기 DB에서 조회한 값으로 치환**합니다.
그게 한 테넌트가 다른 테넌트의 slug로 보내는 것을 막는 유일한 지점입니다.
훅이 없으면 통과(single-tenant 배포에서 맞는 기본값)이고, 그건 신뢰 결정이므로 훅 문서에 그렇게 적어 두었습니다.

`Campaign.TenantVars` 는 캠페인 행에만 있고 **delivery 행에 복사하지 않습니다**. 캠페인은 한 세트를 쓰고,
백만 행에 같은 객체를 백만 번 복사하는 것은 ADR-0002가 캠페인 시작을 한 행 쓰기로 유지한 이유와 같은 이유로 안 합니다.
캠페인이 없는 delivery(트랜잭션, 프로브)만 자기 것을 들고 다닙니다.

### 3. 일반 테넌트는 플랫폼 상태를 보지 않는다

오버레이의 가시성 규칙:

| | 일반 테넌트 | `_system` |
|---|---|---|
| 공유 sender | 보임 (이름, 템플릿 주소, `uses`) — 상태 필드 전부 0, `transport_id`/`domain_id` 는 API에서 생략 | 전부 + 병합된 상태 |
| 공유 transport / domain / mailbox | `List`에 없고 `Get` 은 `ErrNotFound` | 전부 + 병합된 상태 |

sender 프로세스와 프로브/바운스 러너는 공유 transport와 domain을 읽어야 합니다. 그건 **특권 접근자**
`store.PlatformView(ctx, provider)` (= `_system` 스토어)로 합니다. 테넌트용 규칙을 느슨하게 해서 내부를 통과시키지 않습니다.

테넌트가 공유 신원에 대해 듣는 것은 **딱 한 비트**입니다: red인지 아닌지. 이유는 항상 일반 문구
`shared sender unavailable` 입니다. "공유 도메인에서 dkim=fail" 은 운영자의 진단이고, 모두가 공유하는 평판의
한 조각을 한 테넌트에게 알려주는 일이기 때문입니다.

### 4. 바운스 주소는 짧게 두고, 테넌트는 deliveryID로 찾는다

VERP 리턴 패스는 지금 포맷 그대로입니다: `bounce+<deliveryID>.<mac8>@<domain>` (§10).
돌아오는 길의 모든 MTA를 통과해야 하므로 **테넌트를 넣을 자리가 없습니다**.
그런데 **공유 바운스 메일박스**의 메일은 어느 테넌트 것이든 될 수 있고, 메일박스는 그걸 모릅니다.

deliveryID는 압니다. UUIDv7이라 테넌트를 넘어 유일하기 때문입니다.
그래서 `Provider.LookupDeliveryTenant(ctx, deliveryID)` 를 추가했습니다.
shared 모드는 PK 조회 하나, routed 모드는 Provider마다 fan-out(그 비용은 인터페이스 문서에 적어 두었습니다.
그런 Provider는 자기가 발급하는 ID에 샤드를 인코딩하는 편이 낫습니다).

공유 메일박스의 처리 순서: `X-Sendplane-ID` (테넌트를 들고 오는 유일한 상관관계) → 없으면 VERP deliveryID(**미검증**)
→ `LookupDeliveryTenant` → `ForTenant` → **그 테넌트의 키로 VERP HMAC 검증** → 이후는 기존과 동일.
위조된 ID는 실제 테넌트로 해석되고 거기서 unverified로 기록됩니다 — 테넌트 자기 메일박스에 위조 VERP가 왔을 때와 같은 결과입니다.

### 5. sender 사용 정책은 커스터마이즈 가능하다

`PlatformSender.Uses` (`campaign` | `transactional` | `probe`)가 설정에서 오고,
`host.Hooks.SenderPolicy` 가 그걸 대체합니다. 기본 구현은 `host.DefaultSenderPolicy(platform)` 로 export되어 있어
호스트 훅이 **체이닝**할 수 있습니다("무료 플랜은 공유 sender로 캠페인 못 보냄" 같은 규칙은 요금제를 아는 호스트만 쓸 수 있습니다).
`POST /campaigns`(생성과 시작 모두), `POST /messages`, 프로브 트리거에서 강제하고, 거부는 `403 sender_use_denied` 입니다.

### 6. 공정 분배와 공유 suppression

공유 transport의 `rate_per_second` 는 **릴레이 전체의 용량**이므로 버킷을 `_system` 으로 키잉합니다
(테넌트별로 키잉하면 테넌트 수만큼 곱해집니다). 그 위에 `per_tenant_rate_per_second` 로 **(transport, tenant) 버킷**이
하나 더 올라갑니다 — 캠페인 하나가 릴레이를 다 먹는 것을 막는 §8.2의 공정 분배입니다.

`sys:` transport로 나간 메일의 hard bounce / complaint는 테넌트 목록과 함께 **`_system` suppression 목록**에도 올라가고,
`sys:` transport로 보낼 때는 **두 목록을 다 확인**합니다. `_system` 목록은 시스템 테넌트에서만 보이고 관리됩니다.
테넌트가 자기 suppression을 껐어도 이 검사는 합니다: 그 목록은 모두가 공유하는 릴레이를 보호하는 것이고 테넌트의 것이 아닙니다.

## 기각한 대안

### (A) 테넌트 테이블 + 공유 설정 upsert — 기각

가장 짧은 길이었습니다. `tenant` 테이블에 이름/slug를 넣고, 시작할 때(혹은 테넌트 첫 접근 때)
공유 transport/domain/sender를 **모든 테넌트에 upsert** 합니다. 기존 코드가 한 줄도 안 바뀝니다.

기각 이유가 넷이고, 각각 독립적으로 치명적입니다.

1. **설정이 DB로 복제됩니다.** 릴레이 비밀번호가 테넌트 수만큼 DB에 사본으로 존재하고, 한 테넌트의 DB 덤프에
   다른 테넌트가 쓰는 자격증명이 들어갑니다. 비밀번호를 바꾸면 N행 마이그레이션이고, 그 마이그레이션이 실패한 테넌트는
   조용히 옛 비밀번호로 남습니다 — **설정과 DB가 불일치할 수 있는 상태**가 생깁니다.
2. **upsert가 언제 도는지가 정답이 없습니다.** 시작할 때 돌면 새 테넌트는 다음 재시작까지 공유 sender가 없습니다.
   테넌트 첫 접근 때 돌면 읽기 경로에 쓰기가 생기고(ADR-0006이 `TenantSettings` 하나에만 허용한 것),
   그 쓰기가 실패하면 GET이 실패합니다. 주기적으로 돌면 운영자의 설정 편집과 upsert가 서로를 덮습니다.
3. **테넌트 테이블은 두 번째 진실 원천입니다.** 호스트 DB에 이름과 slug가 있고, 여기도 있습니다.
   둘은 반드시 갈라집니다(테넌트 이름 변경, 삭제, 병합). 그리고 갈라진 순간 sendplane 쪽이 낡은 이름으로 메일을 보냅니다.
   ADR-0006이 "테넌트 생명주기를 관리하지 않는다"로 정한 것을 정면으로 되돌리는 결정입니다.
4. **행은 편집 가능합니다.** 테넌트가 자기 테넌트의 공유 transport 행을 PUT해서 host를 자기 서버로 바꿀 수 있습니다.
   막으려면 "이 행은 편집 불가" 플래그가 필요하고, 그 플래그를 들고 있는 행은 이미 가상 엔티티의 나쁜 근사입니다.

가상 엔티티 + shadow 행은 이 넷을 전부 없앱니다. 대가는 `store.WithPlatform` 오버레이 한 파일과
다섯 리포지터리의 래퍼이고, `store/platformtest` 가 그걸 지킵니다.

### (B) 테넌트마다 전용 relay/domain을 요구 — 기각

가장 깨끗하지만 SaaS 요구 자체를 부정합니다. 막 가입한 테넌트가 자기 도메인을 워밍업하기 전에
비밀번호 재설정 메일을 보낼 방법이 없으면 제품이 성립하지 않습니다.

### (C) 공유 sender의 `From` 을 고정 주소로 — 기각

`no-reply@mail.example.com` 하나로 모두가 보내면, 바운스가 테넌트별로 갈라지지 않고
한 테넌트의 나쁜 리스트가 모두의 평판을 깎습니다. 받는 사람도 누가 보냈는지 모릅니다.
`sender+{{ tenant.slug }}@` 는 sub-addressing 하나로 그 둘을 다 해결합니다.

### (D) 테넌트 ID를 VERP 주소에 넣어서 조회를 없애기 — 기각

`bounce+<tenant>.<deliveryID>.<mac8>@` 는 조회를 없애지만 로컬 파트가 테넌트 ID 길이만큼 길어집니다.
§10이 주소를 짧게 유지하기로 한 이유(중간 MTA의 로컬 파트 길이 제한, 잘림)가 그대로 유효하고,
테넌트 ID는 호스트가 정하는 것이라 길이 상한이 없습니다. PK 조회 하나가 훨씬 쌉니다.

## 결과 / 재검토 조건

- `store.Provider` 에 `LookupDeliveryTenant` 가 추가되었습니다. 커스텀 Provider는 구현해야 합니다(storetest가 확인).
- 다섯 모델에 `Shared bool` 이, `Campaign` 과 `Delivery` 에 `TenantVars` 가 추가되었습니다(마이그레이션 `0006`).
  `shared` 컬럼이 참인 행은 shadow 행뿐이고, 설정 컬럼은 전부 빈 값입니다.
- 와이어 ID 타입이 바뀌었습니다. transport / sender / sending domain / mailbox의 ID와 그 참조가
  `format: uuid` 에서 `ResourceId`(UUID 또는 `sys:<이름>`)로 바뀌었습니다. 나머지 ID(캠페인, delivery, 템플릿, …)는 그대로 UUID입니다.
- `gate` 가 `_system` 을 더 이상 거부하지 않습니다. 거기에 도달하는 것은 호스트의 `TenantResolver` 결정이고,
  참조 바이너리는 `auth.tenant_header` 의 role을 가진 principal에게만 허용합니다.
  시스템 테넌트는 캠페인을 만들거나 메시지를 보낼 수 없습니다(422).
- **재검토 조건**: 공유 자원의 수가 설정 파일로 관리하기 어려운 규모(수백 개)가 되면, 설정 파일 대신
  운영자 전용 스토어(sendplane이 아닌 곳)에서 카탈로그를 읽어 `Options.Platform` 을 채우는 형태를 검토합니다.
  오버레이 자체는 그대로 쓸 수 있습니다 — 카탈로그가 어디서 오는지는 `WithPlatform` 이 모릅니다.
