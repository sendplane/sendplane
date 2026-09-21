# test/load

`docs/architecture.md` §15.1 의 **1M 수신자 부하 테스트**입니다. `test/load/docker-compose.yml` 스택
(postgres + control 1 + sender 3 + chaos-smtp 1)을 띄우고, `go run ./test/load` 가 수신자 인제스트부터
최종 단언·리포트까지를 한 번에 수행합니다.

GitHub Actions 는 [`.github/workflows/load-1m.yml`](../../.github/workflows/load-1m.yml) 에서 매일 밤
100만 건으로 돌립니다(러너 예산 45분). **합격 판정은 시간 예산과 정합성 단언만** 하고 처리량(msg/s)은
추세 기록용입니다 — 공용 러너의 절대 수치는 회귀 판정에 쓸 만큼 안정적이지 않습니다.

## 로컬에서 10만 건 돌리기

```sh
make load-test              # = 10만 건 + --kill-sender, 끝나면 스택 정리
make load-test N=1000000    # 진짜 100만 건
```

직접 돌리려면:

```sh
docker build -f deploy/dev/Dockerfile -t sendplane:dev .
docker compose -f test/load/docker-compose.yml up -d
go run ./test/load --recipients=100000 --kill-sender
docker compose -f test/load/docker-compose.yml down -v
```

호스트 포트는 `deploy/dev/docker-compose.yml`(55441/55442)과 겹치지 않게 **postgres 15432,
control 18080, chaos-smtp stats 19090** 을 씁니다. 이미 쓰는 포트가 있으면 환경변수로 바꿉니다.

```sh
LOAD_PG_PORT=15440 docker compose -f test/load/docker-compose.yml up -d
go run ./test/load --recipients=100000 --kill-sender \
  --dsn='postgres://sendplane:sendplane@127.0.0.1:15440/sendplane?sslmode=disable'
```

Compose 플러그인(`docker compose`) 대신 단독 바이너리만 있는 머신에서는 `--compose-cmd=docker-compose`
를 넘겨야 SIGKILL 단계가 동작합니다.

주요 플래그(전체는 `go run ./test/load -h`):

| 플래그 | 기본값 | 의미 |
|---|---|---|
| `--recipients` | 1000000 | 수신자 수 |
| `--chunk` | 50000 | NDJSON 청크 크기 |
| `--replay-chunk` | 7 | 멱등성 검증에 다시 보낼 청크 번호(청크 수보다 크면 마지막 청크) |
| `--ingest-budget` | 3m | 인제스트 예산. 초과하면 단언 실패 |
| `--budget` | 40m | 전체 예산. 초과하면 런 실패 |
| `--kill-sender` | false | 진행률 10%에서 sender 한 대를 SIGKILL 후 재기동 |
| `--seed` / `--tempfail` / `--permfail` / `--drop` | 42 / 0.05 / 0.01 / 0.005 | **compose 파일의 chaos-smtp 설정과 반드시 같아야** 기대값이 맞습니다 |
| `--max-attempts` | 6 | 테넌트 재시도 정책. 런 시작 시 테넌트 설정에 그대로 씁니다 |

## 무엇을 하는가

1. `/healthz` 대기 → 테넌트 설정(트래킹 on, `unsubscribe_mode=sendplane` + 트래킹 도메인, suppression on,
   재시도 백오프 5s/10s/15s/20s/30s, `max_attempts`) → transport(chaos-smtp:2525, TLS none) → sender →
   MJML 템플릿(`{{ recipient.name }}` 한 개 + 추적 링크 한 개) publish → 캠페인 생성.
   **기본 백오프(1m/5m/15m/1h…)를 그대로 쓰면** 일시 실패한 5.5% 가 몇 분씩 대기해 예산을 넘깁니다.
2. NDJSON 을 `--chunk` 줄씩 스트리밍(`Idempotency-Key: chunk-0000` …). 인제스트 시간이 `--ingest-budget`
   을 넘으면 단언 실패.
3. 청크 하나를 **두 번** 다시 보냅니다(§7.2 의 두 가지 보장이 서로 다르기 때문):
   - 같은 `Idempotency-Key` → 저장된 결과가 그대로 돌아오고 `idempotent_replay=true`,
     `accepted` 는 원래 청크 크기 그대로(아무것도 삽입하지 않음).
   - 다른 키 + 같은 본문 → 실제로 다시 인제스트되고 캠페인 내 유니크 인덱스에 전부 걸려
     `accepted=0, duplicates=청크 크기`.
4. `start` → 5초마다 폴링하며 상태별 카운트 출력. `--kill-sender` 면 10% 지점에서 sender 컨테이너 하나를
   `docker kill --signal=KILL` 하고 `--kill-downtime` 뒤에 다시 올립니다(lease 회수 경로).
5. 완료 후 단언 → `report.json` + Markdown 요약.

## 단언 목록

| 단언 | 근거 |
|---|---|
| `sent + failed + suppressed == N`, 진행 중 상태(`pending/queued/leased/deferred`) 0, 캠페인 `completed` | §15.1 |
| `sent` / `failed` 가 **기대값과 정확히 일치** | 아래 "기대값 유도" |
| chaos-smtp `Accepted == sent` | 중복 발송 검출 |
| `Accepted - sent` = `duplicates_from_recovery`. `--kill-sender` 일 때만 `N` 의 0.01% 까지 허용, 아니면 0 | at-least-once 는 SIGKILL 복구 경로에서만 허용 |
| chaos-smtp `TempFailed/PermFailed/Dropped` 가 기대값과 일치(SIGKILL 시 같은 허용치) | attempt 단위 정합성 |
| 실패 delivery 표본 200건이 전부 `attempt_count == max_attempts` 이거나 `last_error_class ∈ {permanent, policy}` | §15.1 |
| 청크 재전송 결과(위 3번) | §7.2 |
| 인제스트 < `--ingest-budget`, 전체 < `--budget` | §15.1 |
| 캠페인에 추적 링크가 최소 1개 | 링크 재작성 경로를 실제로 탔는지 |

## 기대값은 어떻게 나오는가

`chaossmtp.Decide(seed, rates, rcpt, attemptNo)` 는 `SHA256(seed ‖ lower(rcpt) ‖ 0x00 ‖ attemptNo)` 의
상위 53비트를 `[0,1)` 로 정규화해 `tempfail → permfail → drop` 누적 구간에 떨구는 **순수 함수**이고,
sender 는 `X-Sendplane-Attempt` 헤더로 시도 번호를 실어 보냅니다. 그래서 스냅샷을 저장하지 않고
**같은 함수를 호출해 기대값을 계산**합니다(`expect.go`). 수신자 수·실패율·재시도 정책을 바꾸면 기대값도 따라옵니다.

재현하는 sender 정책은 `internal/sender/policy.go` + `classify.go` 입니다.

| chaos 응답 | 분류 | 결과 |
|---|---|---|
| `451 4.3.0 Temporary local problem, try again later` | `rate_limited`(`ratelimit.4xx.text` 규칙이 "try again later" 로 먼저 잡습니다) | attempt 소비, `deferred` |
| DATA 중 연결 끊김 | `transient`(`transient.connection`) | attempt 소비, `deferred` |
| `550 5.2.0 Mailbox unavailable` | `permanent`(`permanent.5xx`) | 즉시 `failed`, attempt 소비 안 함 |
| `250` | — | `sent` |

`attemptNo` 는 1부터 시작하고(`AttemptCount + 1`), 소비한 attempt 수가 `max_attempts` 에 도달하면
`deferred` 대신 `failed` 입니다. 따라서 수신자 한 명의 결과는 `Decide(…, 1), Decide(…, 2), …` 를
`Accept`/`PermFail` 이 나오거나 `max_attempts` 에 닿을 때까지 따라가면 결정됩니다.

**SIGKILL 이 기대값을 흔들지 않는 이유**: 커밋 전에 죽어 다시 보낸 메시지도 같은 `(rcpt, attemptNo)` 로
같은 판정을 받습니다. 그래서 `sent`/`failed` 는 그대로 정확히 맞고, 중복은 chaos-smtp 의 `Accepted` 에만
나타납니다 — 그게 `duplicates_from_recovery` 입니다. drop 은 **기록 전에** 연결을 끊기 때문에
`Accepted` 를 부풀릴 수 없고(`internal/chaossmtp/README.md`), 그래서 `Accepted > sent` 는 언제나 진짜
이중 발송입니다.

## 산출물

`report.json` (워크플로우가 아티팩트로 업로드) 과 같은 내용의 Markdown 요약(`--summary` 로
`$GITHUB_STEP_SUMMARY` 에 append). 처리량(msg/s), 인제스트 시간과 행/초, 실행 시간, 상태별 카운트,
기대값, chaos-smtp 카운터, `duplicates_from_recovery`, `created_at → sent_at` 표본의 p50/p95,
`delivery_attempt` 행 수, DB 크기, 단계별 소요 시간, 실패한 단언 목록이 들어갑니다.

`delivery_attempt` 행 수는 **리포트만 하고 단언하지 않습니다**. `MarkSent` 가 SMTP 250 직후에 먼저
기록되고 `DeliveryAttempt` 행은 그 뒤 배치 `Complete` 에서 쓰이기 때문에(§8.1), 그 사이에 sender 가
SIGKILL 되면 "sent 인데 attempt 행이 없는" 배달이 몇 건 남습니다. 실제로 10만 건 + SIGKILL 런에서
105,756 / 105,770 (14건 부족)이 나왔습니다 — 정상입니다.

`created_at → sent_at` 은 §15.1 의 "claim → sent p95" 를 밖에서 근사한 값입니다. `created_at` 이
인제스트 시각이라 캠페인 규모에서는 대기 시간이 지배하며, 추세용 지표로만 읽으세요.

## 실측 예시 (로컬 10만 건)

2026-09-21, `make load-test`(`--kill-sender`, 6코어 3개 sender)를 한 번 돌린 결과입니다. **한 번의 로컬 측정치**이고
회귀 판정에 쓰는 숫자가 아닙니다(§15.1) — 재현하려면 위 "로컬에서 10만 건 돌리기"를 그대로 실행하세요.

| 지표 | 값 |
|---|---|
| 인제스트 | 10.9s (9,155 rows/s, 예산 180s) |
| 발송 구간 | 288.2s, 평균 346 msg/s |
| sent / failed / suppressed | 98,915 / 1,085 / 0 (기대값과 정확히 일치) |
| chaos-smtp accepted / tempfail / permfail / dropped | 98,915 / 5,256 / 1,085 / 517 |
| SIGKILL 복구로 인한 중복 발송 | 0건 |
| `delivery_attempt` 행 수 | 105,721 / 105,770 (49건 부족, 위 문단과 같은 원인) |
| `created_at → sent_at` p50 / p95 (n=193) | 149.2s / 237.4s |
| DB 크기 | 191.4 MiB |
| 전체 소요 | 311.3s |
