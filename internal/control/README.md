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
| `retention` | 1h | `RetentionDays` 지난 완료/취소 캠페인의 delivery + 테넌트의 tracking/bounce/디스패치된 outbox 행을 5k 청크로 삭제 |
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

## 테넌트 집합과 linger

`Provider.ActiveTenants()`는 "비종료 delivery가 있는 테넌트 **또는** `scheduled|running|paused` 캠페인이 있는 테넌트"입니다.
뒤쪽 절이 control을 위한 것입니다 — 캠페인에 가장 중요한 틱은 *마지막 delivery가 종료된 직후*인데, delivery 쪽만 보면
그 순간 테넌트는 이미 active가 아닙니다. 캠페인이 `running`인 동안 계속 보이므로 finalizer가 그 틱을 놓치지 않습니다.
`_system`(`store.SystemTenantID`)은 절대 반환되지 않습니다.

그래서 여섯 루프 중 **다섯은 유예 기간이 없습니다.** active 집합에서 빠지면 그 자리에서 상태를 버립니다.
`outbox`만 `linger`(3틱)를 씁니다: 캠페인을 `completed`로 옮기는 그 전이가 `campaign.completed`를 enqueue하면서
동시에 테넌트를 active 집합에서 빼기 때문에, 유예가 없으면 그 이벤트가 다음 캠페인 때까지 pending으로 남습니다.
이건 창(window)이지 보장이 아닙니다 — dispatch가 실패한 이벤트는 outbox 백오프를 타고, 그 사이 테넌트가 계속
idle이면 다음 캠페인 때 나갑니다. 완전히 idle한 테넌트의 outbox까지 비우려면 스토어가 "pending 이벤트가 있는 테넌트"를
열거할 수 있어야 하는데, 계약에 그런 것은 없습니다.

## retention이 지우는 것

`RetentionDays`가 지난 완료/취소 캠페인의 delivery(캠페인별 청크) + 테넌트 전체의 TrackingEvent(§9.4),
BounceEvent(§16), 그리고 **이미 디스패치된** outbox 행. pending outbox 행은 아무리 오래돼도 지우지 않습니다 —
아직 호스트에게 줄 빚이고, `failed`는 호스트가 조회·재전송하는 dead letter입니다.
테넌트 설정은 `store.LoadTenantSettings`로 읽습니다(행이 없으면 기본값으로 만들고 읽습니다).

## store 계약에 없어서 못 한 것

1. **`TrackingEvent`에 `Unique` 필드가 없습니다.** `SetFirst*`의 `changed`를 이벤트 행에 기록할 곳이 없어서
   유니크 판정은 `CountUnique`(distinct delivery)에만 의존합니다.
2. **캠페인 없는(transactional) delivery의 보존기간 경로가 없습니다.** `DeleteBefore("")`로 지울 수는 있지만
   "언제 끝났는지"의 기준이 될 캠페인이 없어 지금은 건드리지 않습니다.
3. **"pending 이벤트가 있는 테넌트"를 열거할 수 없습니다.** 위의 outbox linger가 그 대용입니다.

## 루트 패키지와의 연결

`Hooks`/`Event`/`EventSink`는 leaf 패키지 `host`에서 옵니다(architecture §2.1). 루트가 `RunControl`을 구현하려고
이 패키지를 import해도 사이클이 생기지 않고, 루트가 `host` 타입들을 별칭으로 재노출하므로 호스트 쪽 표면은 그대로입니다.
`New(provider, host.Hooks, logger, clock, ...)`에 `sendplane.Hooks`를 그대로 넘길 수 있습니다 — 같은 타입입니다.
