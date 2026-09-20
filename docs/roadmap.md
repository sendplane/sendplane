# sendplane 구현 로드맵

각 Phase는 독립적으로 머지·검증 가능한 단위로 쪼갰습니다. "완료 기준"은 자동화된 테스트로 확인할 수 있는 것만 적었습니다.

## Phase 0 — 뼈대와 스토어 계약 (기반)
1. 모듈 레이아웃, `sendplane.New` 시그니처와 `Options/Hooks/Principal/Action` 타입만 정의 (구현 없음, 컴파일만).
2. `store` 인터페이스 + 모델 정의. `storetest` 스위트 골격(테넌트 격리, 페이지네이션 헬퍼).
3. Postgres 구현: 마이그레이션 프레임, Tenant/Transport/Sender/Layout/Template 리포지터리.
4. `ci.yml`: lint, unit, storetest(postgres) — **완료 기준**: 매트릭스가 녹색.

## Phase 1 — 콘텐츠와 렌더링
1. `internal/render`: Liquid 엔진 래핑, 비활성 태그, `{% t %}` 태그/필터, 폴백 체인, HTML 사전 이스케이프, `$html` 값.
2. i18n 번들 모델 + YAML import/export + 키 추출(`/i18n/keys`).
3. MJML 컴파일(mjml-go) + Layout 슬롯 병합 + HTML→text 자동 생성 + MessageVersion publish.
4. API: Layouts/Templates CRUD, publish, preview. OpenAPI 스펙 작성 → oapi-codegen.
   **완료 기준**: golden 테스트(동일 입력 → 동일 html/text/subject), 누락 키·이스케이프·폴백 케이스, MJML 컴파일 벤치(1회 < 500ms).

## Phase 2 — 캠페인, 인제스트, transactional
1. Campaign/RecipientChunk/Delivery/Attempt 리포지터리(Postgres) + storetest 케이스(멱등 삽입, CAS 전이, claim 무중복, lease 회수).
2. NDJSON 인제스트(`internal/ingest`): 스트리밍 파서, 정규화, 배치 삽입, Idempotency-Key.
3. 캠페인 상태기계 API(start/pause/resume/cancel/retry) + control 리더 루프(스케줄러, 완료 판정, 집계).
4. `POST /messages` transactional + Idempotency-Key.
   **완료 기준**: 100k 인제스트 통합 테스트(청크 재전송 포함) < 30s 로컬, 상태 전이 표 기반 테스트, 리더 페일오버 테스트.

## Phase 3 — Sender
1. claim 루프, 테넌트 라운드로빈, lane 분리, running 캠페인 집합 캐시.
2. Transport 풀(go-mail smtp), 연결 재사용, DKIM 서명 옵션, MIME 조립(List-Unsubscribe/-Post, X-Sendplane-ID, VERP Return-Path).
3. 에러 분류 테이블 + 재시도 정책 + AIMD rate limiter + transport 헬스/서킷.
4. `cmd/chaos-smtp`(결정적 실패 주입) + `e2e.yml`(10k 캠페인, sender SIGKILL 시나리오).
   **완료 기준**: e2e에서 기대 sent/failed 수 정확 일치, 중복 발송 0(정상 종료 시), lease 회수 후 완주.

## Phase 4 — 바운스, suppression, 이벤트
1. DSN/ARF 파서(픽스처 기반 단위 테스트, 실제 Gmail/Outlook/Postfix DSN 샘플 수집).
2. IMAP(go-imap v2)/POP3 폴러 + 메일박스 lock + VERP HMAC 검증.
3. Suppression 리포지터리 + sender 연동 + 테넌트 설정.
4. EventOutbox + webhook 디스패처(HMAC 서명, 재시도, dead-letter, 재전송 API) + Go `EventSink`.
   **완료 기준**: e2e에 테스트 IMAP 서버(예: Dovecot 컨테이너 또는 Go 인메모리 IMAP)로 DSN 주입 → 상태 전이/이벤트/suppression 검증.

## Phase 5 — 트래킹 · 수신거부
1. 서명 토큰(HMAC, kid 회전) + 공개 라우트 `/t/o|c|u` + 버퍼 배치 기록 + delivery `first_*` 조건부 갱신.
2. sender 렌더 후 링크 재작성/픽셀 삽입, `unsubscribe_mode` 3종, `List-Unsubscribe(-Post)` 헤더, publish 시 링크 목록 추출.
3. 호스트 통지 API(`/campaigns/{id}/unsubscribes`, `/deliveries/{id}/unsubscribe`), `Hooks.Unsubscribed`, `recipient.unsubscribed` 이벤트, 봇 판정.
4. 캠페인 통계에 오픈/클릭/수신거부 유니크 수·비율, `GET /campaigns/{id}/links`.
   **완료 기준**: 토큰 위조/오픈 리다이렉트 거부 테스트, e2e에서 픽셀·클릭·원클릭·호스트 통지 후 비율이 기대값과 일치, 스캐너 패턴이 `suspected_bot`으로 제외됨.

## Phase 6 — 발신 헬스 (루프백 + DNS)
1. `ProbeMailbox` 모델/API, `lane=probe` 발송, IMAP 회수(`X-Sendplane-Probe` 검색), 15분 타임아웃.
2. `Authentication-Results`(RFC 8601) 파서 + `Received` 체인 파서(TLS/첫 홉 IP/PTR) + 폴더 판정. 실제 Gmail/Outlook/Postfix 헤더 픽스처.
3. `internal/dnscheck`: SPF/DKIM/DMARC/MX/PTR(miekg/dns, 가짜 리졸버), 관측 IP 기반 힌트.
4. ProbeRun 이력, 요약 상태, `sender.health_changed` 이벤트, 주기 실행 단일성, 캠페인 start 경고.
   **완료 기준**: e2e에서 chaos-smtp가 Authentication-Results를 붙여 IMAP에 배달하는 pass/fail/spam 시나리오가 각각 green/red/yellow로 판정, DNS 체커 픽스처 테스트.

## Phase 7 — MongoDB 동등성
1. Mongo Provider 전체 구현, 인덱스 부트스트랩, claim 전략.
2. `ci.yml` 매트릭스에 mongo 추가, e2e nightly에 mongo 추가.
   **완료 기준**: storetest 100% 통과, e2e 통과.

## Phase 8 — 프론트엔드
1. `@sendplane/api` 생성 파이프라인 + drift 검사.
2. `@sendplane/ui` 기반(주입 컨텍스트, 디자인 토큰, i18n) + 운영 페이지(캠페인 상세: 상태·오픈·클릭·수신거부율·링크별 클릭 / Deliveries / Transports / Sender 헬스(루프백 결과·DNS 힌트) / Events / Suppressions).
3. **스파이크(1~2일, 코드 폐기 전제)**: GrapesJS-MJML을 Vue 3/Vite에서 로드, i18n 태그 블록 삽입, MJML 내보내기 확인 → ADR-0009 확정.
4. 템플릿 편집기(blocks/mjml/html 탭, i18n 패널, YAML import/export, 서버 미리보기).
5. `@sendplane/console` + 참조 바이너리 embed.
   **완료 기준**: 컴포넌트 단위 테스트, 페이지 스토리, 호스트 앱 임베딩 예제(`examples/host-vue`)가 빌드됨.

## Phase 9 — 배포와 1M 부하 테스트
1. Helm 차트(control/sender/bounce/migrate, HPA/KEDA 값, ServiceMonitor, `tracking_domain` Ingress).
2. `load-1m.yml`: 아키텍처 문서 §15.1 그대로(트래킹 on 상태로 실행해 링크 재작성 비용 포함). 처음 실행은 예산을 재지 말고 **측정 결과를 아티팩트로 남긴 뒤** 예산(45분)을 확정.
   **완료 기준**: 1M 정합성 단언 통과, 아티팩트 리포트, 회귀 예산 설정.

## 후순위(범위 밖이지만 자리 예약)
- 프로바이더 webhook 바운스 어댑터, transactional 첨부, ICU 복수형, Postgres 파티셔닝/RLS, A/B 테스트.

## 검증 원칙
- 모든 Phase는 storetest 또는 e2e에 최소 하나의 실패-먼저(failing-first) 테스트를 추가한 뒤 구현한다.
- 성능 숫자는 CI 아티팩트로만 주장한다. 문서에 적는 수치는 측정 출처를 병기한다.
