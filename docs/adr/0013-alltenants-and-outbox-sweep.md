# ADR-0013 `AllTenants` 루프와 outbox-sweep

상태: accepted · 2026-09-21

## 맥락
control의 리더 루프 대부분은 `Provider.ActiveTenants()`(비종료 delivery가 있거나 `scheduled|running|paused` 캠페인이 있는 테넌트)만 돈다. 이게 맞는 루프도 있다: scheduler·canceller·leaseReaper는 테넌트가 활동 중일 때만 할 일이 있다.

문제는 **일이 끝난 뒤에** 도착하는 부류다.

- 바운스는 캠페인이 끝나고 며칠 뒤 DSN으로 온다(`internal/bounce`).
- 루프백 프로브 판정은 1~2초 만에 종단 상태가 되어, 테넌트가 곧바로 `ActiveTenants`에서 빠진다(`internal/probe`).
- 완료된 캠페인의 오픈·클릭·수신거부 유니크 갱신(§7.3)도 거의 항상 캠페인이 끝난 **뒤**에 값이 바뀐다.
- 보존기간 정리(retention)는 애초에 "테넌트가 조용해진 지 오래"인 것을 대상으로 한다.
- `campaign.completed` 이벤트는 그 전이 자체가 테넌트를 active 집합에서 빼는 순간 enqueue되므로, active 집합만 도는 디스패처는 자기가 막 밀어낸 테넌트의 이벤트를 그 자리에서 보내지 못한다.

이 루프들이 `ActiveTenants`만 본다면, 대상 테넌트가 "마침 다른 일을 하고 있을 때만" 우연히 처리가 끝난다. 나머지는 다음 캠페인이 그 테넌트에서 시작될 때까지 무한정 미뤄진다(e2e의 `BUG-2`: 한가한 테넌트의 프로브가 영원히 `pending`으로 남는 문제로 실제로 관측됐다).

## 결정
- `Provider`에 `Tenants(ctx) ([]string, error)`를 추가한다: 활성 여부와 무관하게 설정 행이나 설정성 행(transport/sender/도메인/bounce·probe mailbox/layout/template/campaign)이 있는 **전체** 테넌트.
- `control.Loop`에 `AllTenants bool` 옵션을 추가한다. 켜면 그 루프는 `ActiveTenants() ∪ Tenants()`를 돈다 — active 집합은 항상 즉시 포함하고(지금 일이 있는 테넌트가 캐시를 기다리면 안 됨), `Tenants()`는 리더가 30초 캐시(`WithTenantsRefresh`)로 공유한다(비쌀 수 있는 호출이라서).
- `retention`·`finalizer`(§7.3의 완료 후 트래킹 갱신)·`probe-trigger`·`probe-collect`·`outbox-sweep`이 이 옵션을 켠다. `internal/bounce`의 메일박스 폴러는 애초에 control 루프가 아니라 자체 갱신 주기로 `Provider.Tenants()`를 돈다 — 같은 문제, 같은 해법이지만 control 리더 lease를 필요로 하지 않는 위치라 구현이 별도다.
- 이벤트 outbox는 디스패처를 **둘로 쪼갠다**: active 테넌트(+3틱 유예, linger)를 1초마다 도는 기존 디스패처와, **모든 테넌트**를 10초마다 도는 `outbox-sweep`. `ClaimPending`이 lease를 잡으므로 둘이 같은 행을 두 번 보내지 않는다.

## 기각한 대안
- **linger(유예 틱)만으로 충분하다고 보고 outbox에도 `AllTenants` 없이 적용**: outbox의 3틱 유예는 "캠페인이 막 끝난 순간의 이벤트"라는 좁은 창은 덮지만, 바운스(며칠 뒤)·프로브(1분 뒤)처럼 유예 기간보다 훨씬 늦게 도착하는 일에는 쓸모가 없다. 유예를 몇 시간·며칠 단위로 늘리는 것은 사실상 `AllTenants`를 흉내 내는 것이고, 그럴 바에야 정직하게 전체 테넌트를 돈다.
- **테넌트별 이벤트 기반 웨이크업**(예: DSN 도착 시 그 테넌트의 루프를 즉시 깨움): 정확하지만 메일박스 폴러·리더 루프·pub/sub 사이에 새로운 통신 경로가 생긴다. ADR-0002가 이미 "control과 sender는 DB로만 통신"을 정했고, 이 결정도 같은 원칙을 따른다 — 폴링 주기를 조정하는 편이 훨씬 싸다.
- **`ActiveTenants`의 정의 자체를 넓혀 "바운스/프로브/보류 이벤트가 있는 테넌트"까지 포함**: 그러려면 스토어에 "이 테넌트에 처리 안 된 바운스/프로브/이벤트가 있는가"를 값싸게 물을 방법이 있어야 하는데 지금 계약에는 없다(`internal/control/README.md`의 "store 계약에 없어서 못 한 것" 3번). 생기면 `outbox-sweep`류 루프는 그 자체로 필요 없어져야 한다.

## 결과
- 여섯 개 중 다섯 루프(`scheduler`·`canceller`·`leaseReaper`·active-outbox 그 자체)는 여전히 유예가 없다 — active 집합에서 빠지면 그 자리에서 상태를 버린다. `AllTenants`를 켠 루프만 전체를 도는 대가를 치른다.
- `Tenants()`가 비싼 조회가 될 수 있으므로 30초 캐시가 필수다. 캐시 창 안에서 막 생긴 테넌트(첫 transport 설정 등)는 다음 캐시 갱신까지 `AllTenants` 루프에서 보이지 않을 수 있다 — active 집합에는 즉시 잡히므로 실질적인 피해는 없다.
- 새 `AllTenants` 루프를 추가할 때마다 "이 루프가 정말 유휴 테넌트에서도 할 일이 생기는가"를 먼저 확인해야 한다. 그렇지 않은 루프에 붙이면 전체 테넌트 수에 비례하는 불필요한 순회 비용만 늘어난다.
