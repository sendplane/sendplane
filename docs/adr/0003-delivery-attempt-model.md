# ADR-0003 Delivery / DeliveryAttempt 1급 모델과 상태기계

상태: accepted · 2026-09-20

## 맥락
listmonk와 Keila 모두 "일시 실패한 수신자만 재시도" 요구가 오래 열려 있다. 원인은 캠페인이 수신자·카운터를 직접 들고 있어 수신자별 이력이 없기 때문이다.

## 결정
- `Delivery`(수신자 1명 × MessageVersion 1개)와 `DeliveryAttempt`(SMTP 시도 1회)를 분리한다. 상태·재시도 스케줄은 Delivery에, 응답 코드·에러·transport·소요시간은 Attempt에.
- 상태 집합은 `pending, queued, leased, deferred, sent, failed, bounced, complained, suppressed, cancelled`로 고정하고 전이는 CAS로만 한다.
- SMTP/전송 오류는 `transient / rate_limited / permanent / policy / auth` 다섯 클래스로 정규화하고, 클래스별 처리(재시도, 슬로다운, 실패, transport 격리)를 코드가 아닌 정책 테이블로 둔다.
- 재시도 카운트는 transient/rate_limited에만 소모. 수동 재시도는 `retry_generation`을 올려 이력을 보존한다.
- Campaign과 Transactional은 같은 Delivery 파이프라인을 쓰며 `lane`만 다르다.

## 기각한 대안
- **Campaign에 sent/failed 카운터 컬럼**: hot-row 경합과 정합성 문제. 대신 주기 집계 캐시.
- **실패 상태 하나(`failed`)에 메시지만 저장**: UI에서 "재시도 가능한 것만 다시 보내기"가 불가능.

## 결과
- 캠페인당 수신자 수만큼 Delivery 행이 생긴다(1M 캠페인 = 1M 행 + attempt 행). 보존기간 삭제가 필수 운영 기능이 된다.
- 바운스/이벤트/감사가 모두 delivery ID를 키로 연결된다.
