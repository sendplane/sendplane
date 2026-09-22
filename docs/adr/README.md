# Architecture Decision Records

번호 순으로 읽으면 됩니다. 형식: 맥락 → 결정 → 기각한 대안과 이유 → 결과/재검토 조건.
상태가 `proposed`인 것은 확인이 필요한 결정입니다. 현재 모두 `accepted`이고, 0004와 0008은 상태 줄에
사용자 확인 근거를 남겨 뒀습니다(각 ADR의 "상태" 줄 참조). 결정 이후 구현 과정에서 메커니즘이 더 구체화된
경우 새 ADR을 만드는 대신 해당 ADR에 **보완(addendum)** 절을 추가합니다(0002 참고).

| # | 제목 | 상태 |
|---|---|---|
| 0001 | 라이브러리 우선 + 참조 바이너리 | accepted |
| 0002 | 스토어가 곧 큐: control/sender는 DB로만 통신 (+ 보완: `pending` 직접 클레임) | accepted |
| 0003 | Delivery / DeliveryAttempt 1급 모델과 상태기계 | accepted |
| 0004 | Liquid 템플릿, `{% t %}` i18n 태그, HTML 기본 이스케이프 | accepted (사용자 확인: Liquid 채택) |
| 0005 | 수신자는 NDJSON 스트리밍, 연락처 DB 없음 | accepted |
| 0006 | 멀티테넌시: shared / routed Provider | accepted |
| 0007 | 리포지터리 인터페이스 + storetest 적합성 스위트 | accepted |
| 0008 | 바운스 상관관계(VERP+헤더)와 선택적 내장 suppression | accepted (사용자 확인: 내장 suppression 기본 on) |
| 0009 | MJML은 publish 시 컴파일, 블록 편집기는 GrapesJS-MJML | accepted |
| 0010 | 프론트 패키지 분리 (api / ui / console) | accepted |
| 0011 | 트래킹과 수신거부: sendplane 경유 URL, 서명 토큰, 2단계 수신거부 | accepted |
| 0012 | 헬스체크는 루프백 프로브가 1차, DNS 검사는 진단 계층 | accepted |
| 0013 | `AllTenants` 루프와 outbox-sweep | accepted |
| 0014 | 밀리초 타임스탬프 계약 | accepted |
| 0015 | 메일박스 헬스는 프로브/바운스 결과와 분리 | accepted |
| 0016 | 프로브 수신 채널: IMAP + 전역 inbound webhook | accepted |
| 0017 | 플랫폼 자원은 설정에서 코드로 해석, 테넌트 속성은 요청 변수 | accepted |
