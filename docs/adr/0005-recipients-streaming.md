# ADR-0005 수신자는 NDJSON 스트리밍, 연락처 DB 없음

상태: accepted · 2026-09-20

## 맥락
sendplane은 자체 email DB가 없고, 호출 시 모든 주소를 넘기며, 1M+에서 동작해야 한다.

## 결정
- 캠페인은 `draft`로 만들고 `POST /campaigns/{id}/recipients`에 `application/x-ndjson` 본문을 **스트리밍**으로 여러 번 append 한다. 한 줄 = 수신자 1명(email, name, locale, vars, unsubscribe_url).
- 서버는 줄 단위 디코드 → 2,000행 배치 삽입. 메모리는 배치 크기에 비례.
- 멱등성은 두 겹: `Idempotency-Key`(청크 단위 결과 캐시) + `(campaign_id, email_norm)` 유니크(행 단위).
- 수신자 데이터는 Delivery 행으로만 존재하고 캠페인 보존기간과 함께 삭제된다. 캠페인 간 재사용 개념 없음.
- Transactional은 `POST /messages`에 최대 1,000명까지 인라인.

## 기각한 대안
- **JSON 배열 단일 요청**: 1M×~150B ≈ 150MB를 완전히 파싱해야 하고, 실패 시 전체 재전송.
- **파일 업로드(CSV→오브젝트 스토리지)**: 스토리지 의존성 추가. UI CSV 업로드는 클라이언트가 NDJSON으로 변환해 같은 엔드포인트를 쓰면 된다.
- **"수신자 소스 URL"을 서버가 pull**: SSRF 표면과 인증 문제.

## 결과
- 인제스트 처리량이 캠페인 준비 시간을 결정한다(목표: 1M < 3분 on CI 러너). Postgres는 COPY 경로, Mongo는 insertMany.
- 호스트는 자기 DB에서 커서로 읽어 스트리밍하면 되므로 호스트 측 메모리도 상수.
