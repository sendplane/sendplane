# store/mongo

`store.Provider`의 MongoDB 구현(shared 모드). 모든 문서가 `tenant_id`를 가지며
`ForTenant`가 돌려주는 `store.Store`는 모든 필터에 그 테넌트 조건을 먼저 넣습니다
(ADR-0006). 다른 테넌트의 행을 읽으면 `store.ErrNotFound`입니다.

```go
p, err := mongo.Open(ctx, "mongodb://localhost:27017", "sendplane")
// 또는 이미 연결된 클라이언트를 넘길 때(Close는 클라이언트를 끊지 않습니다)
p, err := mongo.NewShared(ctx, client, "sendplane")

err = p.Migrate(ctx)            // 컬렉션·인덱스 생성(멱등)
s, err := p.ForTenant(ctx, "acme")
```

드라이버는 공식 v2(`go.mongodb.org/mongo-driver/v2`)만 사용합니다.

## 테스트 실행

환경변수 `SENDPLANE_TEST_MONGO_URI`가 비어 있으면 테스트는 `t.Skip`으로 건너뜁니다.
설정하면 `sendplane_test` 데이터베이스에 붙어 `Migrate` 후 `storetest.Run`
전체를 돌립니다. 서브테스트마다 새 테넌트를 쓰므로 사이에 정리할 것이 없고,
같은 DB에 연속으로 돌려 `Migrate` 멱등성을 확인할 수 있습니다.

```sh
SENDPLANE_TEST_MONGO_URI='mongodb://localhost:27017' go test -race -count=1 ./store/mongo/...
go test -count=1 ./store/mongo/...        # 환경변수 없이 → SKIP
```

standalone mongod(레플리카셋 아님)로 충분합니다.

## 트랜잭션을 쓰지 않는 이유

계약(ADR-0007)이 **저장소 간 원자성을 요구하지 않습니다.** 그래서 이 구현은
다중 문서 트랜잭션을 전혀 쓰지 않고, 따라서 standalone mongod에서도 그대로
동작합니다. 원자성이 필요한 지점은 세 가지 방식으로 대체합니다.

- **멱등 삽입**: 인제스트는 `InsertMany(ordered:false)` + unique partial 인덱스
  `{campaign_id:1, email_norm:1}`. 중복 키(11000) 오류 건수만 세서
  `inserted = len(batch) - duplicates`를 돌려줍니다. 캠페인이 없는 배송
  (transactional·probe)은 `campaign_id` 필드 자체를 넣지 않으므로 partial
  인덱스에 들어가지 않고 절대 중복 제거되지 않습니다.
- **CAS 전이**: `Complete`/`MarkSent`는 `lease_owner`가 그대로일 때만,
  `Update`는 `version`이 일치할 때만 적용됩니다. `MatchedCount == 0`이면 문서
  존재 여부를 한 번 더 확인해 `ErrNotFound`와 `ErrConflict`를 구분합니다.
- **이벤트 아웃박스**: 상태 전이와 같은 저장소에 이벤트를 넣습니다.

## claim 전략

`Claim`은 architecture 5.3의 3단계 핸드셰이크입니다.

1. 후보 `_id` 목록을 `find` — `tenant/lane/status∈{queued,deferred}/
   next_attempt_at ≤ now` + 캠페인 필터, 정렬은 `priority desc, next_attempt_at asc`,
   `limit` 적용.
2. `updateMany({_id:{$in:ids}, ...같은 조건}, {$set:{status:leased, lease_owner,
   lease_until, claim_token}})` — **이 호출에서만 쓰는 claim token**을 같이 박습니다.
3. `find({_id:{$in:ids}, claim_token: token})`으로 이번 호출이 실제로 가져간
   행만 읽어 돌려줍니다.

경합에서 후보 일부를 다른 워커에게 뺏기는 것은 허용합니다(다음 루프에서 가져감).
**같은 행이 두 번 나가는 것은 허용하지 않습니다.** token을 쓰는 이유가 그것으로,
같은 `WorkerID`로 두 개의 `Claim`이 동시에 돌아도 서로의 행을 읽지 않습니다.
`Outbox.ClaimPending`도 같은 핸드셰이크를 씁니다.

`ReleaseExpiredLeases`/`BulkTransition`/`Requeue`/`DeleteBefore`처럼 "최대 N건"이
필요한 연산은 MongoDB의 `updateMany`에 limit이 없으므로 같은 방식(후보 id 먼저
고르고 같은 조건을 건 채 갱신)으로 처리합니다.

## 저장 형식

- `_id`는 집계의 ID 문자열(UUIDv7). 자체 ID가 없는 집계(tenant settings,
  suppression, lock, worker, recipient chunk)는 `tenant\x00...` 형태의 복합 키를
  씁니다.
- enum은 int32, `[]byte`·`json.RawMessage`는 BSON binary, `vars`는 BSON 문서.
- **시간은 BSON date가 아니라 Unix epoch 기준 int64 나노초**입니다. BSON date는
  밀리초 해상도라 호출자가 넘긴 타임스탬프를 조용히 반올림하는데, 계약은
  `time.Time`을 그대로 돌려주기를 요구합니다(conformance suite가 `Equal`로
  비교). 영시각(계약의 NULL)은 `null`로 저장합니다.
- 목록은 `(created_at, _id)` 키셋 페이지네이션이고 커서는 base64(불투명)입니다.
  페이지 사이에 삽입된 행이 이미 읽은 행을 밀어내지 않습니다.
