# ADR-0006 멀티테넌시: shared / routed Provider

상태: accepted · 2026-09-20

## 맥락
기본은 `tenant_id` 컬럼 분리, 필요 시 테넌트별 별도 DB 커넥션. sender는 모든 테넌트의 작업을 처리해야 한다.

## 결정
- `store.Provider.ForTenant(tenantID) Store`가 유일한 진입점. 반환된 `Store`는 테넌트에 바인딩되어 있어 리포지터리 메서드에 tenantID 인자가 없다.
- 구현 두 가지: `shared`(단일 DB, 모든 테이블 `tenant_id`, 스코프 래퍼)와 `routed`(tenantID→Provider 함수 + 커넥션 LRU). routed는 미매핑 테넌트를 shared로 폴백할 수 있다.
- 테넌트 식별은 HTTP에서 `TenantResolver`(기본: Principal.TenantID)로, 백그라운드에서는 `Provider.ActiveTenants()`로.
- 테넌트 설정(재시도 정책, suppression, 보존기간, URL 템플릿)은 `TenantSettingsRepo`에 저장하며 sendplane은 테넌트의 생성/삭제를 관리하지 않는다(첫 접근 시 기본 설정 생성).

## 기각한 대안
- **모든 리포지터리 메서드에 tenantID 인자**: 누락 시 교차 테넌트 누출. 타입 수준에서 막는 편이 낫다.
- **Postgres 스키마 per tenant**: 마이그레이션 N배, Mongo와 비대칭. routed 모드가 같은 격리를 더 단순하게 제공.
- **RLS만으로 격리**: Postgres 전용. 보조 수단으로 남겨둔다.

## 결과
- sender는 테넌트 라운드로빈 + 테넌트별 동시성 상한으로 공정성을 확보한다.
- routed 모드에서 `ActiveTenants()`는 호스트가 준 목록(설정/훅)에 의존한다.
