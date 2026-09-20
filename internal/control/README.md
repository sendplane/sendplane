# internal/control

컨트롤 플레인 백그라운드 루프와, API 핸들러가 동기로 호출하는 캠페인 상태 전이입니다.
설계 근거는 [architecture.md §2, §4.1, §7.1, §7.3, §9.3, §12, §16](../../docs/architecture.md),
[ADR-0002](../../docs/adr/0002-store-as-queue.md), [ADR-0003](../../docs/adr/0003-delivery-attempt-model.md).

## 두 가지 실행 단위

| | 어디서 도나 | 무엇이 보장하나 |
|---|---|---|
| 리더 루프 6종 | **리플리카 1대** | `LockRepo` lease (`_system` 테넌트의 `control-leader`) |
| `TrackingBuffer` | **모든 리플리카** | 픽셀/리다이렉트 핸들러가 받은 이벤트를 자기 메모리에 쌓음 |

리더는 `ttl/3`마다 갱신하고, **갱신이 실패하면 루프를 먼저 멈춘 뒤** 다시 획득을 시도합니다.
lease를 잃은 리플리카가 쓰기를 계속하는 구간을 없애기 위해서입니다.

## 루프

각 루프는 `Tick(ctx, now) error` 하나짜리 타입이고, 리더가 `ActiveTenants()`를 돌며
테넌트별 인스턴스에 틱을 겁니다. 모든 루프는 **멱등이고 청크 단위**입니다 — 100만 행 중간에서
죽어도 다음 틱이 이어서 합니다.

| 루프 | 기본 주기 | 하는 일 |
|---|---|---|
| `scheduler` | 5s | `scheduled` 중 `ScheduleAt <= now` → `running` (+`campaign.started`). `draft`는 절대 자동 시작하지 않음 |
| `finalizer` | 10s | `running`마다 `CountByStatus` → `UpdateStats`. `pending/queued/leased/deferred`가 0이면 `completed` + `campaign.completed` |
| `canceller` | 2s | `cancelled` 캠페인의 `pending/queued/deferred`를 틱당 10k씩 `cancelled`로. `leased`는 sender가 끝내도록 둠 |
| `leaseReaper` | 30s | `ReleaseExpiredLeases(now, 5000)` — ADR-0002의 at-least-once 복구 경로 |
| `retention` | 1h | `RetentionDays` 지난 완료/취소 캠페인의 delivery를 5k 청크로 삭제 |
| `outboxDispatcher` | 1s | `ClaimPending` → `EventSink.Emit` → `MarkDelivered` / `MarkFailed`. 워커 풀 기본 8 |

### finalizer의 적응형 주기

100만 행 `COUNT`를 10초마다 도는 것이 이 패키지에서 실제 DB를 아프게 할 수 있는 유일한 쿼리입니다.
delivery 수가 `WithLargeCampaign(rows, every)`의 `rows`(기본 10만)를 넘으면 `every`틱(기본 6)에 한 번만 셉니다.

`total == 0`은 완료로 보지 않습니다. 보존기간이 이미 행을 지웠거나 인제스트와 경합한 캠페인이지,
"할 일이 있었고 다 끝난" 캠페인이 아닙니다.

### outbox 백오프

`1m · 5m · 30m · 2h · 12h`(마지막 값 반복), **10회 실패 후 `failed` 고정 = dead letter**.
`Hooks.Events`가 nil이면 이 루프를 **등록하지 않습니다**. 아무도 소비하지 않는 행의 시도 횟수만 태우기 때문입니다.

## TrackingBuffer (§9.3)

`Record()` → 메모리 → 1초마다(또는 5k 쌓이면) **테넌트당 `InsertEvents` 1회** → `SetFirst*`로 유니크 파생.
크래시 시 최대 1초분 유실은 문서화된 비용입니다.

- `suspected_bot` 이벤트는 **저장하되 `first_*`를 쓰지 않습니다.** 스캐너가 `first_opened_at`을 선점하면
  그 캠페인의 유니크 수치는 되돌릴 수 없습니다.
- `unsubscribe_clicked`(GET)는 `SetUnsubscribed`를 호출하지 않습니다. 확정은 `unsubscribed`뿐입니다(ADR-0011).
- 10만 건을 넘으면 **오래된 것부터 버리고** `Stats().Dropped`에 셉니다. 스토어가 죽었을 때 트래킹이 OOM이 되면 안 됩니다.

## 캠페인 상태기계 (§4.1 / §7.1)

`Control`의 동기 헬퍼들이 강제합니다. 허용되지 않는 전이는 전부 `ErrInvalidTransition`입니다.

```
draft ──start(미래)──► scheduled ──(scheduler)──► running ──pause──► paused ──resume──► running
  │    └─start(즉시/과거)──────────────────────────► running
  │                                                   │
  └──────────── cancel ────────────────────────► cancelled        running ──(finalizer)──► completed
       (draft | scheduled | running | paused 에서만)                completed ──retry──► running
```

- `StartCampaign` 전제조건: delivery ≥ 1, `SenderID`가 실제로 존재, `VersionID` 설정 — 아니면
  `ErrNoRecipients` / `ErrNoSender` / `ErrNoVersion`.
- `pause`/`resume`은 **캠페인 행 하나만** 씁니다. delivery 100만 행은 건드리지 않고, sender의 "running 캠페인 집합"이
  claim 조건으로 쓰입니다(ADR-0002).
- `cancel`도 행 하나만 쓰고 `CompletedAt`을 찍습니다(보존기간 기준). 실제 행 이동은 `canceller` 루프가 합니다.
- `RetryCampaign`은 `RetryGen`을 올려 이력을 보존합니다(ADR-0003). 상태 필터가 비어 있으면 `failed`만
  대상으로 합니다 — 빈 필터는 방금 자기가 큐에 넣은 행을 다시 집어 세대만 계속 올립니다.
- `completed`에서 retry하면 `running`으로 되돌립니다. running 집합 밖의 `queued` 행은 영원히 claim되지 않기 때문입니다.

## 테스트

```
go test -race ./internal/control/...
```

memstore + 고정 시계를 씁니다. 리더 테스트만 실제 시간을 씁니다(lease TTL이 곧 잠드는 시간이라 가짜 시계로는 티커가 안 돌아갑니다).

## store 계약에 없어서 못 한 것

1. **`Provider`에 전체 테넌트 열거가 없습니다.** `ActiveTenants()`는 "비종료 delivery가 있는 테넌트",
   즉 sender용 목록입니다. control에 정말 필요한 틱은 *마지막 delivery가 종료된 직후* — 그 순간 테넌트는
   이미 active가 아닙니다. 지금은 active 집합에서 빠진 테넌트를 `lingerTicks`(3) 동안 더 틱해서 막고 있지만,
   그 사이에 리더가 재시작하면 캠페인이 `running`에 남습니다. `Provider.Tenants()` 또는
   `ActiveTenants`가 "미완료 캠페인이 있는 테넌트"까지 포함하는 쪽이 옳습니다.
2. **`SystemTenantID`가 `store`에 없습니다.** 리더 lock은 테넌트가 없는 클러스터 싱글턴인데 `Store`는
   테넌트 바인딩이라, 이 패키지에서 `"_system"`을 정의해 씁니다. `store.DefaultTenantID` 옆에 있어야
   커스텀 `Provider`가 이 스코프를 실제 고객 테넌트로 오해하지 않습니다.
3. **`TrackingEvent`에 `Unique` 필드가 없습니다.** `SetFirst*`의 `changed`를 이벤트 행에 기록할 곳이 없어서
   유니크 판정은 `CountUnique`(distinct delivery)에만 의존합니다.
4. **`TrackingRepo` / `BounceRepo` / `OutboxRepo`에 `DeleteBefore`가 없습니다.** 그래서 `retention`은
   delivery만 지웁니다. §9.4는 TrackingEvent가 Delivery와 같은 보존기간을 따른다고, §16은 BounceEvent도
   대상이라고 적고 있으므로 세 리포지터리 모두 `DeleteBefore(before, limit)`가 필요합니다.
5. **캠페인 없는(transactional) delivery의 보존기간 경로가 없습니다.** `DeleteBefore("")`로 지울 수는 있지만
   "언제 끝났는지"의 기준이 될 캠페인이 없어 지금은 건드리지 않습니다.

## 알려진 구조 문제: import 사이클

이 패키지는 `Hooks`/`Event`/`EventSink` 때문에 루트 `sendplane` 패키지를 import합니다.
루트가 `Sendplane.RunControl`을 구현하려면 `internal/control`을 import해야 하므로 **그 시점에 사이클이 됩니다.**
`internal/api`도 같은 문제를 갖게 됩니다. 해법은 `Event`/`EventSink`를 `store`(또는 별도 leaf 패키지)로 옮기는 것입니다.
옮기면 이 패키지에서 바뀌는 곳은 `New`의 시그니처와 `events.go`의 `eventFromOutbox` 두 군데뿐입니다.
