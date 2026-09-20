# ADR-0011 트래킹과 수신거부: sendplane 경유 URL, 서명 토큰, 2단계 수신거부

상태: accepted · 2026-09-20 (ADR-0001의 "URL을 만들지 않음" 부분을 대체)

## 맥락
수신거부 처리(구독 상태 변경, 확인 페이지)는 호스트 책임이지만, 캠페인별 수신거부율을 알려면 클릭이 sendplane을 거쳐야 한다. 오픈/클릭 트래킹도 필요하다. 다만 호스트가 자기 URL을 직접 넣는 방식도 선택할 수 있어야 한다. 수신자 DB가 없으므로 수신자별 링크 테이블을 두지 않고도 동작해야 한다.

## 결정
- 테넌트별 `tracking_domain`으로 공개 라우트 `/t/o`(오픈), `/t/c`(클릭), `/t/u`(수신거부 GET 리다이렉트 / POST 원클릭)를 제공한다.
- 토큰은 `delivery_id, kind, link_no, 목적지 URL`을 테넌트 키로 HMAC 서명한 **무상태 토큰**. 목적지가 서명에 포함되어 오픈 리다이렉트를 막고, 수신자별 저장이 없다.
- `unsubscribe_mode ∈ {sendplane, host, none}`. `sendplane`은 메일에 sendplane URL을 넣고 클릭 후 호스트 목적지로 302, 원클릭 POST는 훅/이벤트로 호스트에 통지. `host`는 호스트 URL을 그대로 넣고 호스트가 `POST /campaigns/{id}/unsubscribes`로 통지한다.
- 수신거부는 `unsubscribe_clicked`(GET)와 `unsubscribed`(POST 또는 호스트 통지)로 구분하고, 비율은 확정값으로 계산한다.
- 트래킹 이벤트는 버퍼 배치 삽입, delivery 행의 `first_*` 컬럼은 조건부 갱신으로 유니크 집계.

## 기각한 대안
- **수신거부는 호스트 URL만, 통계는 호스트가 계산**: 호스트마다 구현이 달라지고 캠페인 화면에서 비율을 볼 수 없다. `host` 모드로 여전히 선택 가능하게 남김.
- **수신자별 링크 테이블(짧은 ID)**: URL이 짧아지지만 1M × 링크 수 행이 생기고 인제스트/렌더 경로에 쓰기가 추가된다.
- **GET 수신거부를 즉시 확정 처리**: 링크 스캐너가 대량 수신거부를 만든다(Keila의 protected links 이슈). GET은 호스트 확인 페이지로 넘기고 POST/API만 확정.
- **트래킹 이벤트 동기 삽입**: 오픈 폭주 시 DB 쓰기 병목. 1초 유실을 허용.

## 결과
- 호스트는 `recipient.unsubscribed` 이벤트(또는 `Hooks.Unsubscribed`)를 처리해 자기 DB를 갱신해야 한다. 이것이 계약이다.
- `tracking_domain`을 control로 라우팅하는 DNS/Ingress 설정이 배포 요구사항에 추가된다.
- 오픈률은 MPP로 왜곡되므로 UI는 클릭 지표를 우선한다.
