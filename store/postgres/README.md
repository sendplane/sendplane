# store/postgres

`store.Provider`의 PostgreSQL 구현입니다. shared 모드(ADR-0006)로 동작합니다:
DB 하나, 모든 테이블에 `tenant_id` 컬럼, `ForTenant(tenantID)`가 돌려주는 `store.Store`는
자기가 발행하는 모든 statement에 `tenant_id = $n`을 붙입니다. 다른 테넌트의 행은
언제나 `store.ErrNotFound`입니다.

ORM이나 쿼리 빌더는 쓰지 않습니다. SQL은 그것을 쓰는 Go 코드 옆에 그대로 있습니다.

## 사용

```go
p, err := postgres.Open(ctx, dsn)      // 풀을 직접 만들고 소유(Close가 풀도 닫음)
// 또는
p, err := postgres.NewShared(ctx, pool) // 이미 있는 풀을 감쌈(Close가 풀을 닫지 않음)

if err := p.Migrate(ctx); err != nil { ... }
s, err := p.ForTenant(ctx, "tenant-1")
```

`Migrate`는 `migrations/*.sql`을 `embed`로 넣어 두고 `schema_migrations` 테이블에
적용 여부를 기록합니다. 멱등이며, advisory lock을 잡으므로 control 레플리카가
동시에 기동해도 DDL이 한 번만 돕니다. 외부 마이그레이션 라이브러리는 쓰지 않습니다.

## 테스트

적합성 스위트(`store/storetest`)는 DSN 환경변수가 있을 때만 돕니다.

```
SENDPLANE_TEST_POSTGRES_DSN='postgres://sendplane:sendplane@localhost:5432/sendplane?sslmode=disable' \
  go test -race -count=1 ./store/postgres/...
```

환경변수가 비어 있으면 `t.Skip`으로 조용히 넘어가므로, PostgreSQL이 없는 머신에서도
`go test ./...`가 통과합니다. 스위트는 서브테스트마다 새 테넌트를 쓰기 때문에
서브테스트 사이에 truncate가 필요 없고, 같은 DB에 여러 번 연달아 돌려도 됩니다.

## claim 쿼리

`DeliveryRepo.Claim`은 architecture 5.2의 CTE 하나입니다.

```sql
WITH c AS (
  SELECT id FROM delivery
   WHERE tenant_id = $1 AND lane = $2 AND status IN (1, 3)   -- queued, deferred
     AND next_attempt_at <= $3
     AND (campaign_id IS NULL OR campaign_id = ANY($4))
   ORDER BY priority DESC, next_attempt_at, id
   LIMIT $5 FOR UPDATE SKIP LOCKED)
UPDATE delivery d SET status = 2, lease_owner = ..., lease_until = ..., updated_at = ...
  FROM c WHERE d.id = c.id RETURNING d.*;
```

- `FOR UPDATE SKIP LOCKED` 덕분에 sender 레플리카 수십 개가 동시에 돌아도 같은 행을
  두 번 집지 않습니다(`storetest`의 `ClaimConcurrent`가 16 워커 × 2,000건으로 확인).
- `ClaimRequest.CampaignIDs`의 세 경우를 그대로 옮깁니다: `nil`이면 캠페인 필터 없음,
  길이 0인 non-nil이면 `campaign_id IS NULL`, 값이 있으면
  `campaign_id IS NULL OR campaign_id = ANY(...)`.
- 시각은 `req.Now`를 씁니다(`now()` 아님). 테스트와 호출자가 시간을 통제합니다.
- `UPDATE ... RETURNING`은 행 순서를 보장하지 않으므로, 계약이 요구하는
  `priority DESC, next_attempt_at` 순서는 Go에서 다시 정렬해 돌려줍니다.
- 인덱스: `delivery_claim (tenant_id, lane, priority DESC, next_attempt_at) WHERE status IN (1,3)`.

## COPY 경로

`InsertBatch`는 배치 크기로 경로를 고릅니다(`WithCopyThreshold`로 조정, 기본 100행).

- 100행 이하: multirow `INSERT ... ON CONFLICT DO NOTHING`.
- 100행 초과: 트랜잭션 안에서 `CREATE TEMP TABLE (LIKE delivery) ON COMMIT DROP` →
  `pgx.CopyFrom` → `INSERT INTO delivery SELECT ... FROM tmp ON CONFLICT DO NOTHING`.
  삽입된 건수는 이 `INSERT`의 command tag에서 가져옵니다.

두 경로 모두 멱등입니다. 캠페인 배달은 부분 유니크 인덱스
`delivery_campaign_email (tenant_id, campaign_id, email_norm) WHERE campaign_id IS NOT NULL`로
중복이 걸러지고, 같은 배치 안의 중복은 Go에서 먼저 접습니다(같은 statement가 삽입 중인
행은 `ON CONFLICT`가 볼 수 없기 때문). 캠페인이 없는 배달(transactional, probe)은
`campaign_id`가 NULL이라 인덱스 밖에 있고 절대 중복 제거되지 않습니다.

"캠페인 없음"은 SQL에서 NULL, Go에서 빈 문자열입니다. 조회는
`campaign_id IS NOT DISTINCT FROM $n::text`로 두 경우를 한 번에 다룹니다.

## 그 밖의 메모

- **낙관적 동시성**: `version bigint` 컬럼 +
  `UPDATE ... WHERE id = $1 AND tenant_id = $2 AND version = $3`. 0행이면 행 존재를
  다시 확인해 `ErrConflict`(버전 불일치)와 `ErrNotFound`(다른 테넌트/삭제됨)를 구분합니다.
- **커서 페이지네이션**: `(created_at, id)` keyset을 base64로 감싼 불투명 커서입니다.
  페이지를 읽은 뒤 삽입된 행은 커서 뒤에 정렬되므로 기존 행을 건너뛰거나 두 번 주지
  않습니다. suppression은 id가 없어 `(created_at, email_norm)`을 씁니다.
- **`Complete`**: 결과 배치를 `UPDATE ... FROM unnest(...)` 한 방에 적용하고
  `lease_owner`로 CAS합니다. 실제로 매치된 행만 `RETURNING id`로 돌려받아 그 행에만
  attempt를 넣으므로, 리스를 잃은 결과와 재전송된 배치는 조용히 무시됩니다. 행 단위
  루프는 없습니다.
- **시간 정밀도**: `timestamptz`는 마이크로초까지입니다. 계약은 호출자가 준
  `time.Time`을 그대로 돌려주므로(`storetest`가 `Equal`로 비교), `delivery`의
  호출자 지정 시각에는 `*_ns smallint` 컬럼을 짝지어 0~999ns 나머지를 보관합니다.
  다른 테이블은 스스로 시각을 찍으므로 마이크로초가 공식 해상도입니다.
- **enum**은 `smallint`, 구조화된 값은 `jsonb`, ID는 `text`(UUIDv7 문자열)입니다.
  숫자 enum 값은 on-disk 포맷의 일부입니다(`store/enums.go`).
