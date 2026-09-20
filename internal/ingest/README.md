# internal/ingest

캠페인에 수신자를 **NDJSON 스트리밍**으로 append 하는 패키지입니다.
설계 근거는 [architecture.md §7.1, §7.2](../../docs/architecture.md), [ADR-0005](../../docs/adr/0005-recipients-streaming.md).

## 흐름

```
POST /campaigns/{id}/recipients  (application/x-ndjson, Idempotency-Key: chunk-0007)
  → Ingester.Ingest(ctx, campaignID, chunkKey, body)

  1. Campaigns.Get          → draft | scheduled 만 허용 (아니면 ErrCampaignNotEditable)
  2. Deliveries.CountByStatus 합계 = 현재 수신자 수 (캠페인 행에 카운터를 두지 않음, §7.3)
  3. chunkKey != ""         → RecipientChunks.Get
                               completed → 저장된 카운트를 즉시 반환 (본문을 읽지 않음)
                               그 외      → pending 으로 Put 하고 계속
  4. 줄 단위 디코드 → 검증 → 2,000행마다 Deliveries.InsertBatch
  5. chunkKey != ""         → completed + 카운트로 Put
  → Result{accepted, duplicates, invalid, total, errors}
```

`Result.Total`은 **호출이 끝난 뒤의 캠페인 수신자 수**(`CountByStatus` 합계)입니다.

## 멱등성 두 겹

| 층 | 수단 | 재전송 시 |
|---|---|---|
| 청크 | `Idempotency-Key` → `store.RecipientChunk` | `completed`면 저장된 카운트를 즉시 반환. **본문을 읽지 않습니다** |
| 행 | `(campaign_id, email_norm)` 유니크 | 중간에 실패한 청크를 통째로 다시 보내도 이미 들어간 행은 `duplicates`로 집계 |

에러로 끝난 호출은 청크를 `pending`으로 **남겨 둡니다**. 재전송이 본문을 다시 흘려보내도 유니크 인덱스가 막아주기 때문입니다.
`chunkKey`가 비면 청크 레코드를 만들지 않습니다.

## 줄 단위 검증

| 항목 | 규칙 | 실패 시 |
|---|---|---|
| 줄 길이 | `Limits.MaxRecipientLineBytes` (기본 64 KiB) | `ErrLineTooLong` |
| JSON | 줄 하나 = JSON 오브젝트 하나 | `ErrBadJSON` |
| email | `store.NormalizeEmail` (소문자화, IDN→punycode, CR/LF 거부, `"Name <a@b>"` 허용) | `ErrBadEmail` |
| name | trim 후 CR/LF 거부 (헤더 인젝션) | `ErrBadName` |
| locale | `^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$` | `ErrBadLocale` |
| vars | 수신한 JSON 바이트 크기 ≤ `Limits.MaxVarsBytes` (기본 8 KiB) | `ErrVarsTooLarge` |
| unsubscribe_url | 있으면 절대 http(s) URL | `ErrBadUnsubscribeURL` |

**잘못된 줄은 호출을 실패시키지 않습니다.** `Invalid`에 세고 `Errors`(최대 100건, `WithMaxLineErrors`)에 줄 번호와 함께 담은 뒤 건너뜁니다.
빈 줄과 공백뿐인 줄도 건너뜁니다. 줄 번호는 빈 줄을 포함해 1부터 셉니다.

호출 전체를 실패시키는 것은 둘뿐입니다: `ErrCampaignNotEditable`(API → 409), `ErrTooManyRecipients`(API → 413).
그리고 스토어/네트워크 에러. 이때도 `Result`에는 그 시점까지 들어간 카운트가 담겨 돌아옵니다.

## 중복 집계

- **배치 안**: `email_norm` 맵으로 거르고 스토어에 보내지 않습니다. 맵은 배치마다 비웁니다(메모리를 O(batch)로 유지).
- **배치 밖 / 이전 호출**: `InsertBatch`가 돌려준 `inserted`와 배치 크기의 차이가 곧 중복 수입니다.

## 메모리

본문 크기와 무관하게 상수입니다: bufio 읽기 버퍼 64 KiB + 줄 버퍼(줄 길이 상한) + 배치 2,000행.
`bufio.Scanner`를 쓰지 않은 이유는 첫 초과 줄에서 스트림 전체가 `bufio.ErrTooLong`으로 죽기 때문입니다.
`lineReader`는 초과한 줄을 끝까지 버리고 다음 줄부터 이어갑니다.

`TestIngestStreams200k`는 200,000줄(약 20 MB)을 `io.Pipe`로 흘려보내며 **살아 있는 힙 최대치**를 샘플링합니다. 측정값은 약 **5 MiB**, 테스트 상한은 64 MiB입니다.
(이 테스트는 delivery 리포지터리를 카운터로 바꿔 둡니다. 200k 행을 memstore에 살려 두면 ingest가 아니라 스토어의 메모리를 재게 됩니다.
20k 행을 실제로 memstore에 넣고 세는 것은 `TestIngestStoresEveryRow`입니다.)

## 한도

`host.Limits`(= `sendplane.Limits`, 같은 타입)의 필드를 그대로 씁니다. 0인 필드는 `Limits.WithDefaults()`가 `host.DefaultLimits`로 채웁니다.
`host`는 루트 `sendplane` 대신 import하는 leaf 패키지입니다 — 루트가 `Handler`를 구현하려고 `internal/api`를
import하는 순간 반대 방향이 사이클이 되기 때문입니다(architecture §2.1).

> 주의: `MaxRecipientsPerCampaign`의 기본값은 `host/limits.go`의 **10,000,000**입니다.
> 한도에 정확히 도달한 캠페인에서는 이어지는 줄이 중복이더라도 `ErrTooManyRecipients`가 됩니다.
> 중복 여부를 가리려면 줄마다 스토어 왕복이 필요한데, 한도에 딱 걸터앉은 캠페인에서만 생기는 경우라 그 비용을 치르지 않습니다.

## CSV

`CSVToNDJSON(r, w, ColumnMapping{})`은 UI 업로드 경로용 변환기입니다(ADR-0005: 서버는 파일을 저장하지 않고, 클라이언트가 NDJSON으로 바꿔 같은 엔드포인트로 보냅니다).
헤더의 `email`(필수) / `name` / `locale` / `unsubscribe_url`을 필드로 매핑하고 **나머지 열은 전부 `vars`의 문자열 값**이 됩니다.
헤더 이름 비교는 대소문자를 무시하고, `ColumnMapping`으로 열 이름을 바꿀 수 있습니다. 숫자·불린을 추측하지 않고, 빈 칸은 `vars`에서 뺍니다.
주소가 빈 행도 그대로 내보냅니다 — 여기서 조용히 버리는 대신 `Ingest`가 줄 번호와 함께 `invalid`로 보고하게 합니다.

## 공개 API

```go
New(st store.Store, limits host.Limits, clock func() time.Time, ...Option) *Ingester
WithBatchSize(n int) Option      // 기본 2000
WithMaxLineErrors(n int) Option  // 기본 100

(*Ingester).Ingest(ctx, campaignID, chunkKey string, r io.Reader) (Result, error)

CSVToNDJSON(r io.Reader, w io.Writer, mapping ColumnMapping) error
```

## 테스트

`BenchmarkIngest200k`은 스토어를 카운터로 대체해 **디코드+검증+배치**만 잽니다.
측정 예(Xeon Gold 6248, go1.25, `-benchmem`): `2.04 s/op · 10,180 ns/line · 342 MB/op · 6.8M allocs/op`.
줄당 10 µs면 1M 수신자가 약 10초로, ADR-0005의 "1M < 3분" 목표 안쪽입니다. 시간의 대부분은 `encoding/json` 디코드와 행당 UUIDv7 생성입니다.

```
go test -race ./internal/ingest/...
go test -run XXX -bench Ingest -benchmem ./internal/ingest/
go test -short ./internal/ingest/    # 20k/200k 스트리밍 테스트 제외
```
