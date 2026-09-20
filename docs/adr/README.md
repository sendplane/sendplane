# Architecture Decision Records

번호 순으로 읽으면 됩니다. 형식: 맥락 → 결정 → 기각한 대안과 이유 → 결과/재검토 조건.
상태가 `proposed`인 것은 확인이 필요한 결정입니다.

| # | 제목 | 상태 |
|---|---|---|
| 0001 | 라이브러리 우선 + 참조 바이너리 | accepted |
| 0002 | 스토어가 곧 큐: control/sender는 DB로만 통신 | accepted |
| 0003 | Delivery / DeliveryAttempt 1급 모델과 상태기계 | accepted |
| 0004 | Liquid 템플릿, `{% t %}` i18n 태그, HTML 기본 이스케이프 | accepted |
| 0005 | 수신자는 NDJSON 스트리밍, 연락처 DB 없음 | accepted |
| 0006 | 멀티테넌시: shared / routed Provider | accepted |
| 0007 | 리포지터리 인터페이스 + storetest 적합성 스위트 | accepted |
| 0008 | 바운스 상관관계(VERP+헤더)와 선택적 내장 suppression | accepted |
| 0009 | MJML은 publish 시 컴파일, 블록 편집기는 GrapesJS-MJML | accepted |
| 0010 | 프론트 패키지 분리 (api / ui / console) | accepted |
| 0011 | 트래킹과 수신거부: sendplane 경유 URL, 서명 토큰, 2단계 수신거부 | accepted |
| 0012 | 헬스체크는 루프백 프로브가 1차, DNS 검사는 진단 계층 | accepted |
