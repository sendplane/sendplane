# ADR-0014 밀리초 타임스탬프 계약

상태: accepted · 2026-09-21

## 맥락
`store` 계약은 Postgres와 MongoDB 양쪽에서 동일하게 성립해야 한다(ADR-0007). 두 백엔드의 시간 표현은 해상도가 다르다: Postgres `timestamptz`는 마이크로초까지 담고, MongoDB의 BSON date는 밀리초가 네이티브 해상도다. `Claim`의 `next_attempt_at <= now` 같은 비교, `ReleaseExpiredLeases`의 lease 만료 판정, 커서 페이지네이션의 `(created_at, id)` 키셋은 전부 저장된 시각과 호출자가 넘긴 `time.Time`을 비교하므로, 두 값이 "같은 순간을 가리키는데 다르게 비교되는" 사고가 나기 쉽다. 실제로 초기 구현에서 이 불일치가 버그로 나타났다(`432c30e fix(store): millisecond timestamp contract, tenant-scoped unique index, atomic InsertBatch`).

## 결정
- 계약의 시간 해상도를 **밀리초**로 못박는다(`store/doc.go`). 모든 구현은 저장 직전 모든 `time.Time`을 `store.TruncateTime`으로 밀리초 단위로 **절삭**한다(반올림이 아니다 — 반올림은 경계값 근처에서 저장값이 원래 순간보다 앞서갈 수 있어 `next_attempt_at <= now` 같은 비교를 뒤집을 수 있다).
- 저장값뿐 아니라 **비교 경계값도 같은 함수를 지난다.** Postgres는 `tsIn`/`tsInNN`에서, Mongo는 쓰기 직전에 동일하게 절삭한다. 그래서 같은 `time.Time`에서 만든 두 값은 구현이 무엇이든 항상 정확히 일치하게 비교된다.
- 영시각(`time.Time{}`)은 SQL/BSON NULL로 대응하고, `TruncateTime`은 영시각을 영시각인 채로 둔다 — `SetFirstOpened`류의 "NULL일 때만 갱신" 조건부 UPDATE가 이 불변식에 의존한다.
- 커서는 저장 해상도와 같은 단위(Unix 밀리초)로 인코딩한다. 디코딩한 값이 원래 행의 `created_at`과 항상 정확히 일치해야 keyset 페이지네이션이 행을 건너뛰거나 반복하지 않는다.

## 기각한 대안
- **나노초/마이크로초까지 보존**: Mongo BSON date가 애초에 밀리초 이하를 버리므로 한쪽만 정밀해 봐야 두 구현이 어긋난다. `storetest`를 두 백엔드에 동일하게 돌리는 것(ADR-0007)이 이 프로젝트의 핵심 전제라, 더 낮은 공통분모에 맞추는 편이 맞다.
- **초 단위로 낮추기**: 구현은 더 단순해지지만 claim 주기(수 초)·lease 기간(수 분)과 같은 자릿수라 재시도 스케줄링과 lease 판정의 정밀도가 눈에 띄게 나빠진다. 밀리초는 두 백엔드의 자연스러운 공통분모이면서 이 문제를 피한다.
- **비교 시점에만 절삭하고 저장은 원본 정밀도 유지**: 저장값과 비교 경계값이 서로 다른 코드 경로를 타면 한쪽만 고치고 잊어버리는 회귀가 나기 쉽다. 두 지점이 같은 함수(`TruncateTime`)를 지나게 만들어 이 클래스의 버그를 구조적으로 차단한다.

## 결과
- 모든 구현이 정밀도를 잃는 대신, `storetest`의 시간 비교 테스트가 두 백엔드에서 동일하게 통과한다는 확실성을 얻는다.
- 밀리초 미만의 정밀도가 필요한 기능(예: 초정밀 지연 측정)은 이 계약 밖에서 별도로 기록해야 한다. 현재 그런 요구는 없다.
- 새 구현을 추가하는 사람은 `TruncateTime`을 쓰기 경로와 비교 경계값 양쪽에 빠짐없이 적용해야 `storetest`를 통과한다 — 이 ADR이 그 요구사항의 근거 문서다.
