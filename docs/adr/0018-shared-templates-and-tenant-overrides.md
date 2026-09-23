# ADR-0018: 공유 템플릿과 테넌트 재정의

- 상태: accepted (사용자 결정: 공유는 템플릿별 opt-in, 재정의는 전체 사본, `key` 는 선택, 공유 템플릿에 `uses` 제한과 정책 훅)
- 날짜: 2026-09-24
- 관련: [ADR-0017](0017-platform-resources-from-config.md) (플랫폼 자원 오버레이), [ADR-0006](0006-multitenancy-provider.md) (멀티테넌시),
  [ADR-0009](0009-mjml-publish-time.md) (publish 시 컴파일), [architecture.md §5.4 · §6](../architecture.md),
  `store/overlay_content.go`, `internal/api/shared.go`, `host/platform.go`

## 맥락

ADR-0017로 운영자는 자기 **인프라**(릴레이, 도메인, 발신 신원)를 테넌트에게 빌려줄 수 있게 되었습니다.
같은 SaaS에서 바로 다음 요구가 **콘텐츠**입니다. 비밀번호 재설정, 가입 환영, 결제 영수증 같은 메일은 모든 테넌트에
거의 같은 모양으로 필요하고, 운영자가 한 번 잘 만들어 두면 테넌트는 그대로 쓰거나 자기 브랜드로 고쳐 쓰고 싶어 합니다.

sendplane에서 템플릿은 테넌트 행입니다(`template`, `layout`, `message_version`). "공유 템플릿"의 가장 짧은 길은
**모든 테넌트에 같은 행을 복제**하는 것이고, ADR-0017에서 설정에 대해 기각한 바로 그 설계입니다.
템플릿은 설정이 아니라 **행**(운영자가 API로 편집하는 데이터)이라는 점만 다릅니다.

요구를 정리하면 다섯 가지입니다.

1. `_system` 테넌트의 템플릿을 **템플릿별로** 공유로 표시할 수 있다(opt-in).
2. 공유 템플릿은 모든 테넌트가 **읽기 전용으로** 쓸 수 있다.
3. 테넌트는 공유 템플릿을 **재정의**할 수 있고, 재정의의 범위는 **전체**다(제목, 본문, 텍스트, 블록, i18n 번들, 레이아웃 참조).
4. 템플릿을 테넌트를 넘어 식별할 **`key`** 가 있고, 그건 **선택**이다.
5. 공유 sender처럼 공유 템플릿에도 **용도 제한**(`uses`)과 정책 훅이 있다.

레이아웃도 같은 규칙을 따라야 합니다. 공유 템플릿의 레이아웃이 모든 테넌트에서 해석되어야 하기 때문입니다.

## 결정

### 1. 공유 콘텐츠는 `_system` 에 한 번 있고, 테넌트는 read-through로 읽는다

`Template` 과 `Layout` 에 `Key`, `Shared` 가, `Template` 에 `Uses`, `OverriddenFromVersion` 이 추가됩니다(마이그레이션 `0007`).
공유 템플릿과 그 publish 버전은 **`_system` 에만** 존재합니다. ADR-0017의 오버레이(`store.WithPlatform`)가
일반 테넌트의 `Templates()` / `Layouts()` / `Versions()` 를 감싸서 읽기 규칙을 적용합니다.
두 번째 래퍼는 만들지 않았습니다 — 같은 "운영자가 테넌트에게 빌려준다" 모델의 두 번째 절반이기 때문입니다.

| 연산 (일반 테넌트) | 규칙 |
|---|---|
| `List` | 테넌트가 재정의하지 **않은** 공유 행 + 자기 행. 공유 행을 가리는 자기 행은 `Overridden` 으로 표시(계산값, 저장 안 함) |
| `Get(id)` | 자기 행 → 없으면 `_system` 행 중 `Shared` 인 것 |
| `GetByKey(key)` | **자기 것 먼저**, 그다음 공유 — 재정의가 이기는 이유 |
| 공유 행 ID로의 쓰기 | `ErrReadOnly` (API `403 platform_read_only`) |
| `Shared: true` 인 자기 행 쓰기 | `ErrInvalid` — 공유는 `_system` 만 |
| `Versions().Get(id)` | 자기 것 → 없으면 `_system` 의 **어떤** 버전이든 |
| `Versions().ListByTemplate(id)` | 지금 공유 중인 템플릿의 ID면 `_system` 버전 목록, 아니면 자기 것 |
| `Versions().Create` (공유 템플릿 ID로) | `ErrReadOnly` — 공유 템플릿의 publish는 `_system` 에서만 |

`_system` 테넌트는 자기 행을 그대로 봅니다. 공유 콘텐츠를 작성하는 곳이기 때문입니다.

오버레이는 이제 **카탈로그가 비어 있어도** 적용됩니다. 공유 템플릿은 설정이 아니라 행이므로,
공유 릴레이 없이 운영하는 배포도 템플릿은 공유할 수 있어야 합니다. 공유된 것도 설정된 것도 없으면 전달만 합니다.

#### 버전 read-through 규칙

"테넌트는 `_system` 버전을 ID로 **아무거나** 읽을 수 있다"는 넓어 보이지만 가장 단순하게 옳은 규칙입니다.

- 버전은 **불변**이고 ID는 UUIDv7이라 열거할 수 없습니다. 목록으로는 절대 넘어가지 않습니다.
- sender는 in-flight delivery가 큐잉될 때의 버전을 **계속** 읽어야 합니다. 템플릿이 그 사이에 공유 해제되어도
  이미 수락된 메일은 나가야 하므로 "지금 공유 중인 템플릿의 버전만" 규칙은 sender를 깨뜨립니다.
- **호출자가 준** 버전 ID(`version_id` 핀, `GET /message-versions/{id}`)는 API가 한 단계 더 좁힙니다.
  `_system` 버전은 그 템플릿이 **지금 공유 중일 때만** 보입니다(`internal/api/shared.go` `visibleVersion`).
  그래서 공유 해제된 템플릿의 버전은 ID를 추측해도 닿지 않습니다.

### 2. `key` 는 선택이고, 테넌트 안에서 유일하다

`^[a-z0-9][a-z0-9._-]{0,63}$`, 빈 문자열은 "키 없음". 유일성은 `(tenant_id, key) WHERE key <> ''` 부분 유니크 인덱스
(Postgres 부분 인덱스, Mongo `partialFilterExpression: {key: {$gt: ""}}`, memstore는 같은 규칙을 코드로).
키가 없는 템플릿은 공유할 수 없고(`422`), 재정의할 수 없고, `template_key` 로 찾아지지 않습니다.

키를 필수로 만들지 않은 이유는 기존 템플릿이 전부 키 없이 존재하고, 대부분의 테넌트 템플릿은 영원히 공유와 무관하기
때문입니다. 필수로 만들면 백필(무엇으로?)과 모든 생성 폼에 의미 없는 필드가 생깁니다.

**재정의는 별도 관계가 아니라 키의 일치**입니다. 테넌트 템플릿의 `Key` 가 공유 템플릿의 `Key` 와 같으면 그것이
재정의입니다. `OverridesTemplateID` 같은 참조를 두지 않은 것은, 참조는 공유 템플릿이 지워지고 같은 키로 다시
만들어지면 끊어지는데 키 일치는 그대로 유지되기 때문입니다.

### 3. 전송은 키로 해석한다

`POST /messages` 와 `POST /campaigns` 가 `template_id` 대신 `template_key` 를 받습니다(둘 다 주면 `422`).
키는 `GetByKey` 로 해석되므로 **테넌트 재정의가 있으면 그것, 없으면 공유 템플릿**입니다.

- 트랜잭션은 **요청 시점에** 해석합니다. 재정의를 만들거나 지우는 즉시 다음 전송이 따라갑니다.
- 캠페인은 **생성 시점에** 해석한 템플릿 ID를 저장하고, 시작 시점의 버전 해석은 지금처럼 ID로 합니다(§7.1).
  공유 템플릿 ID도 read-through로 해석됩니다.

### 4. 재정의는 명시적인 전체 사본이다

`POST /templates/{id}/override` (`template.write`)가 공유 템플릿을 테넌트 소유의 **완전한** 템플릿으로 복사합니다.
key, 이름, subject, preheader, mode, body, blocks, text, i18n 번들, layout 참조, default locale, `uses` 가 복사되고
`Shared=false`, `OverriddenFromVersion` = 공유 템플릿의 현재 `PublishedVersionID` 입니다.
이미 같은 키의 템플릿이 있으면 `409 duplicate`. **재정의를 지우면(`DELETE`) 공유 템플릿으로 돌아갑니다.**

이것이 이 설계에서 유일한 복사이고, 사용자가 **편집할 사본을 달라고 요청했기 때문에** 존재합니다.

두 가지 세부 결정:

- **사본은 공유 버전으로 publish된 상태로 시작합니다.** `PublishedVersionID` 에 공유 템플릿의 버전 ID를 그대로 넣습니다.
  버전은 read-through이므로 복사되는 것은 없고, 재정의 직후 테넌트가 아직 publish하지 않은 동안에도 키로 보내는 메일이
  `422 template_not_published` 로 끊기지 않습니다. 테넌트가 처음 publish하면 자기 버전으로 바뀝니다.
- **"공유본 변경됨"** 은 이력 없이 비교 하나로 압니다. 오버레이가 재정의 행에 공유 원본의 현재 `PublishedVersionID` 를
  계산해 붙이고(`SharedPublishedVersionID`, 저장 안 함), API가 `OverriddenFromVersion` 과 다르면
  `shared_updated_since_override: true` 를 돌려줍니다.

레이아웃도 같습니다(`POST /layouts/{id}/override`). 레이아웃에는 버전이 없으므로 대신 **레이아웃 참조를 키로 해석**합니다:
템플릿의 `layout_id` 가 가리키는 레이아웃에 키가 있으면 publish/preview는 그 키를 자기 것 먼저로 다시 찾습니다.
그래서 테넌트가 공유 레이아웃 `base` 를 재정의하면, 공유 레이아웃 ID를 가리키는 그 테넌트의 모든 템플릿(재정의한
템플릿 사본 포함)이 다음 publish부터 테넌트의 `base` 를 씁니다. 예외는 **테넌트가 보는 공유 템플릿**의 preview로,
`_system` 이 publish한 것과 같은 레이아웃으로 렌더합니다(preview가 실제 발송과 달라지면 안 되므로).

공유 템플릿의 레이아웃은 **공유 레이아웃이어야 합니다**(저장과 publish에서 `422`). 그렇지 않으면 어떤 테넌트도
그 템플릿의 preview나 재정의 사본을 렌더할 수 없습니다.

### 5. 용도 제한과 정책 훅

`Template.Uses` (`campaign` | `transactional`, 비어 있으면 전부)는 `_system` 작성자가 정합니다.
`host.Hooks.TemplatePolicy func(ctx, host.TemplateUse) error` 가 `POST /messages`, 캠페인 생성과 시작에서
템플릿이 해석된 뒤(ID, 키, 핀 버전의 템플릿 어느 쪽이든) 호출되고, 거부는 `403 template_use_denied` 입니다.

```go
type TemplateUse struct {
    TenantID    string
    Principal   *Principal
    TemplateID  string
    TemplateKey string
    Shared      bool        // _system 의 공유 템플릿을 read-through로 쓰는 경우만 true
    Uses        []UseKind   // 템플릿 자신의 uses
    Kind        UseKind
    TenantVars  map[string]any
}
```

기본값 `host.DefaultTemplatePolicy` 는 **공유 템플릿에만** `Uses` 를 강제합니다. 테넌트 자기 템플릿은 제한하지 않습니다.
**재정의 사본은 `uses` 를 복사해 갖지만 테넌트 소유이므로 기본 정책은 제한하지 않습니다.** 운영자가 재정의에도
원본의 제한을 유지하고 싶다면 훅이 그렇게 하면 됩니다(`TemplateKey` 와 `Uses` 가 그 판단에 필요한 전부입니다).
기본값을 이렇게 둔 이유는, 재정의는 테넌트가 명시적으로 자기 것으로 만든 콘텐츠이고 테넌트 자기 템플릿은
원래 무엇에든 쓸 수 있기 때문입니다. SenderPolicy와 같이 훅은 기본값을 **대체**하므로 체이닝합니다(`host.DefaultTemplatePolicy`).

`TemplateUse` 에 `Uses` 가 들어 있는 이유: `DefaultSenderPolicy` 는 카탈로그(설정)에서 uses를 읽지만,
템플릿의 uses는 **행**에 있고 정책 함수는 스토어를 모릅니다. 그래서 해석된 템플릿의 값을 넘깁니다.

캠페인 **시작**에서도 다시 검사합니다. 캠페인이 draft로 며칠 있는 동안 운영자가 공유 템플릿의 uses를 좁힐 수 있기
때문이며, ADR-0017의 sender 재검사와 같은 이유입니다.

## 기각한 대안

### (A) 사용 시 복사(copy-on-use) — 기각

테넌트가 공유 템플릿을 처음 쓸 때(전송, 캠페인 생성) 테넌트로 행을 복사하고 이후에는 그 사본을 쓰는 방식입니다.
기존 코드가 거의 바뀌지 않습니다. 기각 이유:

1. **운영자의 수정이 전파되지 않습니다.** 복사된 순간 테넌트는 옛 버전에 고정됩니다. "모든 테넌트의 영수증 문구를
   고친다"가 N행 마이그레이션이 되고, 그 마이그레이션은 테넌트가 일부러 고친 사본과 그냥 복사된 사본을 구분할 수 없습니다.
2. **읽기 경로에 쓰기가 생깁니다**(ADR-0017 기각안 (A)의 2번과 같은 문제). 전송 요청이 템플릿 행을 만들고,
   그 쓰기의 실패가 전송의 실패가 됩니다.
3. **복사와 재정의가 구분되지 않습니다.** "공유본 변경됨" 을 알려 줄 방법도, 재정의를 지워 공유본으로 돌아갈 방법도
   사라집니다. 둘 다 "테넌트가 명시적으로 만든 사본" 이라는 구분이 있어야 성립합니다.

### (B) i18n만 재정의(번들 오버레이) — 지금은 기각

테넌트가 공유 템플릿의 **번역 문자열만** 덮어쓰는 방식입니다. 사본이 작고 운영자의 구조 변경이 계속 전파된다는
장점이 있습니다. 기각(현재) 이유:

1. 사용자 결정이 **전체 재정의**입니다. 테넌트가 원하는 것은 대부분 자기 브랜드의 레이아웃과 문구 둘 다입니다.
2. 부분 오버레이는 **병합 규칙**을 새로 만듭니다: 공유본이 키를 지우면? 이름을 바꾸면? publish 시점의 병합 결과가
   두 행에 걸쳐 결정되면 "지금 무엇이 나가는가" 를 한 행으로 답할 수 없습니다.
3. 전체 재정의 위에 나중에 추가할 수 있습니다(재정의 사본의 i18n만 편집하는 UI). 반대 방향은 어렵습니다.

재검토 조건: 테넌트 대부분이 문구만 바꾸고 운영자가 구조를 자주 바꾸는 운영 패턴이 확인되면 (B)를 **추가 모드**로 검토합니다.

### (C) 재정의를 참조 컬럼으로 표현 (`overrides_template_id`) — 기각

위 §2 참조. 키 일치가 참조보다 공유 템플릿의 삭제·재생성에 강하고, "키로 보낸다" 와 "재정의가 이긴다" 가
같은 한 가지 규칙(`GetByKey` 자기 것 먼저)이 됩니다.

## 결과 / 재검토 조건

- `TemplateRepo` / `LayoutRepo` 에 `GetByKey` 가 추가되었습니다. 커스텀 Provider는 구현해야 합니다(storetest `ContentKeys`).
- `store.WithPlatform` 은 카탈로그가 비어 있어도 Provider를 감쌉니다. 두 번 감싸면 그대로 돌려줍니다.
- 테넌트의 템플릿 목록은 `_system` 의 템플릿 목록을 **통째로** 읽어 공유된 것을 거릅니다. `_system` 은 운영자의
  수십 개 템플릿을 가진다는 가정입니다. 공유 템플릿이 수천 개가 되면 전용 쿼리(`WHERE shared`)와 인덱스를 추가합니다.
- 공유 템플릿을 공유 해제하면 모든 테넌트에서 즉시 사라집니다. 그 ID로 만든 draft 캠페인은 시작 시 "템플릿 없음" 이 되고,
  이미 큐잉된 delivery는 버전 read-through 덕분에 그대로 나갑니다.
- 공유 레이아웃을 공유 해제하거나 지워도 그것을 쓰는 공유 템플릿을 막지 않습니다(레이아웃 없이 렌더). 필요해지면
  "공유 템플릿이 쓰는 레이아웃은 공유 해제 불가" 검사를 추가합니다.
- e2e 시나리오 9가 실제 Postgres에서 "재정의 외에는 테넌트에 `template` / `message_version` 행이 없다" 를 SELECT로 확인합니다.
