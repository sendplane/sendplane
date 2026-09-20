# ADR-0002 스토어가 곧 큐: control/sender는 DB로만 통신

상태: accepted · 2026-09-20

## 맥락
control과 sender는 별도 프로세스여야 하고, DB는 Postgres/Mongo/커스텀이 가능해야 하며, 메일 단위 재시도와 1M 규모가 요구된다.
별도 메시지 브로커를 넣으면 배포 의존성이 늘고 "delivery 상태의 진실"이 두 곳으로 갈라진다.

## 결정
- **Delivery 행 자체가 큐 항목**이다. sender는 `DeliveryRepo.Claim`(lease 기반)으로 가져가고 `Complete`로 상태 전이를 커밋한다. 처리 중 죽으면 lease 만료 후 다른 sender가 가져간다(at-least-once).
- Postgres는 `FOR UPDATE SKIP LOCKED`, Mongo는 후보 조회 → 조건부 `updateMany` → 소유 조회.
- control과 sender 사이에 RPC 없음. 스케줄/일시정지/취소는 캠페인 행 상태로 전달되며 sender는 "running 캠페인 집합"을 주기 갱신해 claim 조건에 넣는다. 따라서 **start/pause에 1M 행 갱신이 없다.**
- 리더가 필요한 작업(스케줄러, 집계, DNS 체크, 아웃박스)은 `LockRepo`의 lease로 단일 실행을 보장한다.

## 기각한 대안
- **Redis/NATS/Kafka 큐**: 처리량 상한은 더 높지만 (a) 필수 의존성 추가 (b) 상태의 이중화(큐 메시지 vs DB 행) (c) 커스텀 DB 요구와 충돌. SMTP 자체가 초당 수백~수천 건이 상한이므로 DB 큐로 충분하다. `Claim/Complete` 인터페이스가 경계이므로 나중에 교체 가능.
- **start 시 pending→queued 일괄 갱신**: 단순하지만 1M 행 UPDATE가 락/WAL 부담. running 집합 필터로 대체.
- **캠페인 상태를 delivery claim 쿼리에서 JOIN**: 모든 claim마다 조인 비용. sender 측 몇 초 캐시가 충분.

## 결과
- 정확히 한 번 발송은 보장하지 않는다(문서화). 중복 완화를 위해 SMTP 250 직후 즉시 `MarkSent` 옵션을 둔다.
- DB가 병목이 되면 먼저 배치 크기·인덱스·파티셔닝으로 대응하고, 그래도 부족할 때 큐 백엔드 교체를 검토한다(측정 후).
