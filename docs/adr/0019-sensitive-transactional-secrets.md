# ADR-0019: 민감 정보 transactional 발송 — 수명이 정해진 `secret_vars`와 AAD 바인딩 암호화

- 상태: accepted (사용자 확인 2026-11-11: AAD 방향 채택, 런타임 폴백 없음, 이전은 별도 도구)
- 날짜: 2026-11-11
- 관련: [ADR-0002](0002-store-as-queue.md) (스토어가 곧 큐), [ADR-0003](0003-delivery-attempt-model.md) (Delivery 상태기계),
  [ADR-0008](0008-bounce-and-suppression.md) (바운스), [ADR-0011](0011-tracking-and-unsubscribe.md) (트래킹),
  [ADR-0017](0017-platform-resources-from-config.md) (플랫폼 자원), [architecture.md §9 · §10 · §16](../architecture.md),
  `store/platform.go`, `store/delivery.go`, `internal/api/messages.go`, `internal/sender/process.go`

## 맥락

OTP, 비밀번호 재설정 링크, 일회용 초대 토큰처럼 **본문 자체가 비밀인 메일**을 transactional로 보내야 합니다.
요구는 셋입니다: DB 노출 최소화, 발송 후 제거, 재전송 불가.

지금 그런 값을 `vars`로 보내면 셋 다 깨집니다.

| 경로 | 문제 |
|---|---|
| `delivery.vars` | 평문 jsonb 저장 |
| `GET /deliveries…` | `vars` 를 그대로 응답 |
| 보존기간 | campaign 단위로만 삭제 — transactional 행은 **삭제 경로가 없음** (architecture §18 7번) |
| `POST /deliveries/{id}/retry` | 같은 vars로 재발송 |
| 클릭 트래킹 | 토큰이 든 링크가 리다이렉트 URL과 `tracking_event.url` 에 남음 |
| `BounceRetainRaw` | DSN에 붙어 오는 원본 본문이 `bounce_event.raw` 에 남음 |

발송은 비동기입니다(ADR-0002). API가 받은 순간과 sender가 SMTP로 넘기는 순간 사이에, 다른 replica가 읽을 수 있는 곳에
값이 **잠깐은 있어야** 합니다. 문제는 "어디에, 어떤 형태로, 언제까지"입니다.

동시에, 기존 at-rest 암호화(`SecretCipher`)에는 **행 바인딩이 없었습니다**. 참조 구현은 AES-GCM에 AAD `nil` 을 넘겼고,
DB 쓰기 권한이 있으면 테넌트 A의 SMTP 비밀번호 암호문을 테넌트 B의 transport 행에 복사해 B가 A의 계정으로 발송하게 만들 수 있었습니다.
`secret_vars` 에서는 같은 공격이 **A의 OTP를 B의 주소로 보내는 것**이 됩니다.

## 결정

### 1. `SecretCipher` 는 `SecretContext` 를 받는다 (AAD 바인딩)

```go
type SecretContext struct {
    Purpose  string // "transport.password", "sending_domain.dkim_private_key", "delivery.secret_vars", …
    TenantID string
    ObjectID string // 행 ID, 플랫폼 자원은 "sys:<id>"
    Bind     string // 추가 바인딩 — delivery는 수신자(email_norm) 해시
}
func (c SecretContext) AAD() []byte // 버전 태그 + 길이 접두 인코딩

type SecretCipher interface {
    Encrypt(ctx context.Context, plaintext []byte, sc SecretContext) ([]byte, error)
    Decrypt(ctx context.Context, ciphertext []byte, sc SecretContext) ([]byte, error)
}
```

- 컨텍스트는 **sendplane만 조립**합니다(`store` 의 생성자). 호출 지점이 직접 만들지 않습니다.
- `[]byte` 가 아니라 구조체인 이유: KMS 호스트가 필드를 EncryptionContext(`map[string]string`)로 옮길 수 있게.
  AES-GCM 호스트는 `sc.AAD()` 만 넘기면 됩니다.
- `Bind` 에 이메일 원문이 아니라 해시를 넣는 이유: KMS 컨텍스트는 감사 로그(CloudTrail 등)에 평문으로 남습니다.
  바인딩 효과는 해시로 같습니다.
- 모든 필드는 행 수명 동안 불변이어야 합니다.

**런타임 폴백은 없습니다.** "AAD로 실패하면 AAD 없이 재시도"는 다운그레이드 경로이고, 막으려던 행 간 교체를 그대로 열어 둡니다.
기존 암호문은 별도 도구 `sendplane secrets rewrap` (라이브러리: `sendplane.RewrapSecrets`)으로 **한 번** 다시 감쌉니다.
도구는 "새 컨텍스트로 복호화되면 건너뜀"으로 재실행에 안전하고, `--dry-run` 을 지원하며, 평문을 출력하지 않습니다.
설정 파일의 플랫폼 비밀은 부팅 시 메모리에서 암호화하므로(ADR-0017) 대상이 아닙니다.

`SecretCipher` 가 nil이면 기존 비밀(transport/메일박스/DKIM)은 지금처럼 평문 저장을 유지합니다 — 호스트가 비밀을 sendplane 밖에 두는 구성.
**`secret_vars` 는 cipher가 없으면 거부**합니다.

### 2. `secret_vars`: 수신자별, 암호화, TTL, 발송 즉시 삭제

```jsonc
POST /api/v1/messages
{ "to": [{ "email": "a@x.com", "secret_vars": { "code": "482913" } }],
  "secret_ttl_seconds": 600 }
```

- 템플릿은 `{{ secret.* }}` 로만 접근합니다. `vars` 에 섞이지 않습니다.
- 수신자별로만 받습니다. 여러 사람에게 같은 값을 보내는 것은 비밀이 아닙니다.
- 템플릿이 참조하는 `secret.*` 키가 요청에 없으면 **422**. 빈 코드가 든 메일이 나가는 것이 가장 나쁜 결과입니다.
  검사 범위는 subject · HTML · text와 i18n 번역 문자열(번역도 Liquid)입니다.
  반대로 템플릿이 `secret.*` 를 읽지 않는데 `secret_vars` 가 오면 역시 422 — 아무도 쓰지 않는 비밀은 저장하지 않습니다.
- `secret.*` 를 읽는 템플릿은 **sensitive delivery만** 렌더할 수 있습니다. 캠페인 생성·수정·시작은 422로 미리 거절하고,
  최종 방어선은 sender입니다: sensitive가 아닌 delivery의 버전이 `secret.*` 를 읽으면 발송하지 않고 영구 실패로 끝냅니다
  (캠페인은 시작 시점에 발행 버전을 고정하므로 API 검사만으로는 닫히지 않습니다).
- 크기 상한: 수신자당 직렬화 JSON 8 KiB. TTL은 30초~24시간, 기본 600초.
- 저장: `delivery.secret_ct` (SecretContext{Purpose: "delivery.secret_vars", TenantID, ObjectID: delivery ID, Bind: email_norm 해시}로 암호화),
  `secret_expires_at`, `secret_state`.
- `secret_vars` 가 있는 delivery는 **sensitive** 이고, 아래 정책이 끌 수 없이 적용됩니다.

`secret_state` 와 삭제 시점:

| 상태 | 언제 | `secret_ct` |
|---|---|---|
| `pending` | 생성 | 암호문 |
| `consumed` | SMTP 250 (`MarkSent`) | 같은 UPDATE에서 NULL |
| `discarded` | 보내지 않고 terminal (영구 실패, suppressed, 취소) | 같은 쓰기에서 NULL (suppressed는 처음부터 저장 안 함) |
| `expired` | `secret_expires_at` 경과 | control 루프 또는 sender가 terminal(`failed`)로 바꾸며 NULL |

일시 실패(deferred)는 자동 재시도를 위해 TTL까지 보관합니다. 다음 재시도 시각이 만료 이후면 sender가 그 자리에서
`failed` (LastError `secret expired`)로 끝냅니다. 이때 TTL은 아직 남아 있으므로 store는 `secret_state` 를 `discarded` 로 기록합니다 —
`expired` 는 실제로 만료 시각이 지난 뒤 정리된 행에만 붙습니다. 어느 쪽이든 암호문은 같은 쓰기에서 지워집니다.

### 3. 재전송 차단 (겹겹이)

1. API: sensitive delivery의 retry는 `409`.
2. store: `Requeue` 가 sensitive 행을 대상에서 제외 — storetest 계약.
3. sender: `secret_state != pending` 이거나 암호문이 없으면 렌더하지 않고 실패.
4. idempotency 재요청은 원래도 결과만 돌려줍니다.

### 4. 다른 노출 경로

- API/UI: 암호문·평문 필드 없음. `sensitive`, `secret_state`, `secret_expires_at` 만.
- 열람 픽셀·클릭 링크 재작성 안 함 (수신거부 헤더는 비밀과 무관하므로 유지).
- 바운스: sensitive delivery에 상관된 DSN은 `raw` 를 저장하지 않음.
- `RecipientContext.Vars`, 수신거부 URL 템플릿, 이벤트 payload에 `secret` 없음.
- 로그/에러 문자열은 **능동적으로 가립니다**. Liquid 오류(`can't convert string(VALUE) …`)와 누락 키 경고, 본문을 되돌려 주는
  릴레이의 SMTP 응답이 값을 그대로 인용하기 때문입니다. sender가 delivery 결과·attempt 행·렌더 로그에서 비밀 값(원문,
  HTML 이스케이프, 쿼리 이스케이프 형태)을 `[secret]` 로 치환합니다. 한계: 3바이트 미만 값과 릴레이 응답 안의 QP/base64 인코딩은 가리지 못합니다.

## 기각한 대안

- **평문 안에 행 ID를 넣고 복호화 후 대조**: 인터페이스는 안 바뀌지만, 대조를 잊은 경로 하나가 그대로 구멍입니다.
  AAD는 잊어도 복호화가 실패합니다. 기존 비밀도 보호하지 못합니다.
- **선택적 `AEADCipher` 인터페이스(타입 단언)**: 호환은 되지만 바인딩이 선택 사항이 되어 기존 비밀이 결국 무바인딩으로 남습니다.
  v0.0.x 이므로 시그니처를 바꿉니다.
- **호스트 훅으로 발송 시점에 값을 받음(sendplane 무저장)**: 호스트는 보통 OTP를 해시로만 저장합니다.
  훅을 위해 호스트가 평문을 따로 들고 있어야 하면 노출 위치만 옮겨집니다.
- **동기 발송(API에서 바로 SMTP)**: 보장은 가장 강하지만 store-as-queue, 속도 제한, 자동 재시도를 우회하고
  API replica가 SMTP 연결을 갖게 됩니다.

## 결과와 한계

- **WAL·백업·VACUUM 전 튜플**에는 암호문이 남습니다. 평문은 아닙니다. 보호 수준은 transport 비밀번호와 같고, 짧은 TTL이 완화책입니다.
- **같은 행 복원**은 AAD가 막지 못합니다: 백업에서 `secret_ct` 를 같은 행에 되돌리고 상태를 되돌리면 같은 수신자에게 다시 갈 수 있습니다.
  DB 쓰기 권한이 전제이고 평문은 읽지 못합니다. 재전송 차단은 §3의 상태 검사가 맡고, AAD는 기밀성과 행·수신자 바인딩을 맡습니다.
- **at-least-once 구간**: SMTP 250 직후 `MarkSent` 전에 죽으면 lease 만료 후 같은 수신자에게 한 번 더 갈 수 있습니다. 표준 동작을 유지합니다.
- `Hooks.BeforeSend` 는 렌더된 본문(비밀 포함)을 봅니다. 호스트 자신의 코드이므로 범위 안입니다.
- VERP 상관에 실패한 DSN은 sensitive 여부를 알 수 없어 테넌트 설정대로 `raw` 가 저장될 수 있습니다.
- 업그레이드 절차: 새 버전 배포 → senders가 비밀을 쓰기 전에(또는 멈춘 상태로) `secrets rewrap` 실행. 폴백이 없으므로 순서가 중요합니다.

## 재검토 조건

- 호스트가 envelope 암호화(행별 DEK)를 요구하면 `SecretContext` 는 그대로 두고 cipher 구현 쪽에서 해결합니다.
- transactional delivery 보존기간 삭제(§18 7번)가 생기면, sensitive 행의 메타데이터도 그 경로를 따릅니다.
