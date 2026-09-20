# internal/tracking

오픈·클릭·수신거부 **무상태 서명 토큰**과 바운스 상관관계용 **VERP 주소**를 만드는 패키지입니다.
설계 근거는 [architecture.md §9.1, §10, §16](../../docs/architecture.md), [ADR-0011](../../docs/adr/0011-tracking-and-unsubscribe.md).

## 토큰

```
token = kid "." base64url( body ‖ mac )

body = len(tenant_id) ‖ tenant_id ‖ len(delivery_id) ‖ delivery_id ‖ kind ‖ varint(link_no)
       [ ‖ len(dest) ‖ dest ]          // kind == unsubscribe 일 때만
mac  = HMAC-SHA256( secret,
         "sendplane/tracking/v1\0" ‖ len(tenant_id) ‖ tenant_id
         ‖ len(delivery_id) ‖ delivery_id
         ‖ kind ‖ varint(link_no) ‖ len(dest) ‖ dest )[:16]
```

- **목적지(dest)는 kind와 무관하게 항상 MAC에 포함**됩니다. 그래서 `/t/c/{token}?u=...` 는 서명자가 고른 `u` 로만 리다이렉트되고, 오픈 리다이렉트가 성립하지 않습니다(§16).
- 반대로 **토큰 본문에 dest를 싣는 것은 `unsubscribe` 뿐**입니다. `/t/u/{token}` 은 쿼리 없이 호스트 목적지로 302 해야 하고 원클릭 POST도 받아야 하므로 목적지를 스스로 들고 있어야 합니다.
- 길이 접두사가 모든 가변 필드에 붙어 있어 서로 다른 페이로드가 같은 MAC 입력을 만들 수 없습니다.
- **테넌트 ID가 토큰 안에 있습니다.** 공개 라우트는 인증이 없고 서명 키는 테넌트별이라, 토큰에 테넌트가 없으면 검증이 모든 테넌트의 키를 훑어야 합니다. MAC이 테넌트 ID도 덮으므로 다른 테넌트로 바꿔치기할 수 없습니다.

| 함수 | 용도 |
|---|---|
| `Signer.Sign(kid, secret, TokenPayload)` | 토큰 생성. 키가 못 쓸 상태면 빈 문자열(= 트래킹 생략). 렌더 경로에서 링크마다 호출되므로 에러를 반환하지 않습니다 |
| `Signer.SignErr(...)` | 같은 동작 + 에러. sender가 수신자 루프 **전에** 키를 한 번 검사할 때 씁니다 |
| `Signer.Verify(keys, token)` | 오픈·수신거부 라우트용. 클릭 토큰은 목적지 없이 검증하면 임의의 `u`를 받아주게 되므로 **거부**합니다 |
| `Signer.VerifyDest(keys, token, dest)` | 클릭 라우트용. `u` 파라미터를 함께 넘깁니다 |
| `SelectKey(keys)` | 새 토큰에 쓸 키(가장 최근 `CreatedAt`, 동률이면 KID) |
| `OpenURL` / `ClickURL` / `UnsubscribeURL` | `/t/o/`, `/t/c/?u=`, `/t/u/` URL 조립. 도메인이나 토큰이 비면 빈 문자열 |

키 회전은 `TrackingConfig.SigningKeys` 에 옛 키를 남겨 두는 동안만 옛 링크가 살아 있습니다(`ErrUnknownKID`).

## VERP (§10)

```
Return-Path: bounce+{deliveryID}.{hmac8}@{bounce_domain}
```

`VERPAddress(bounceDomain, deliveryID, secret)` / `ParseVERP(addr, keys)`.
MAC 컨텍스트가 트래킹과 다르므로(`"sendplane/verp/v1\0"`) 테넌트가 키를 재사용해도 두 서명이 섞이지 않습니다.
`ParseVERP` 는 **검증 실패해도 delivery ID는 돌려주고 `verified=false`** 로 표시합니다 — §10의 "위조 바운스는 `unverified` 로 기록만" 규칙을 바운스 파서가 구현할 수 있게.

## 문서와 다른 점

| 항목 | 문서 | 구현 | 이유 |
|---|---|---|---|
| 토큰 구분자 | `kid ‖ base64url(...)` | `kid "." base64url(...)` | kid 길이가 가변이라 구분자 없이는 되돌릴 수 없습니다. `.` 은 base64url 알파벳에 없어 충돌하지 않습니다 |
| MAC 길이 | `HMAC-SHA256(...)` | 앞 16바이트 | 128비트면 위조가 무의미하고, 토큰이 모든 메일의 모든 링크에 들어가므로 24자를 아낍니다 |
| `Verify` 시그니처 | `Verify(keys, token)` | 동일 + `VerifyDest(keys, token, dest)` | 클릭 목적지는 토큰 밖(`u=`)에 있어 검증에 반드시 필요합니다. 기본 `Verify` 는 `VerifyDest(…, "")` |
