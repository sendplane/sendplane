# ADR-0008 바운스 상관관계(VERP+헤더)와 선택적 내장 suppression

상태: accepted · 2026-09-21 (사용자 확인: 내장 suppression 기본 on)

## 맥락
바운스는 IMAP/POP3 메일박스에서 읽는다. 바운스 메일을 어떤 Delivery와 연결할지, 그리고 hard bounce 주소를 다시 보내지 않으려면 어딘가에 기록이 있어야 하는데 sendplane은 "email DB 없음"이 원칙이다.

## 결정
- 상관관계 근거를 세 겹으로 심는다: VERP Return-Path(`bounce+{deliveryID}.{hmac8}@bounce-domain`), `X-Sendplane-ID` 헤더, `Message-ID`. DSN(RFC 3464)/ARF(RFC 5965) 파서 + 휴리스틱 폴백. HMAC 불일치는 `unverified`로 기록만 한다.
- **내장 suppression을 테넌트 설정으로 제공하고 기본 on.** 저장 항목은 `email_norm, reason(hard_bounce|complaint|manual), source_delivery_id, created_at, expires_at`뿐. sender는 claim 후 렌더 전에 조회해 `suppressed` 처리한다.
- 호스트가 자체 관리하고 싶으면 끄고 `BeforeSend` 훅 또는 사전 필터링을 사용한다. 모든 바운스/컴플레인은 이벤트로 호스트에 전달되므로 호스트 DB가 진실이 될 수 있다.

## 기각한 대안
- **suppression 완전 미제공**: 호스트가 이벤트를 놓치면 같은 hard-bounce 주소로 계속 보내 도메인 평판이 손상된다. 발송 엔진의 책임 범위라고 판단.
- **Message-ID만으로 상관관계**: 많은 MTA가 DSN에 원본 헤더를 넣지 않는다.
- **suppression을 해시로 저장**: 매칭은 가능하지만 UI에서 조회/해제가 불가능. 보존기간으로 대신 관리.

## 결과
- bounce 도메인의 MX가 폴링 대상 메일박스를 가리켜야 VERP가 동작한다(문서화).
- 릴레이가 envelope sender를 덮어쓰는 환경에서는 헤더 경로만 남는다.
