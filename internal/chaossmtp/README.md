# internal/chaossmtp

**일부러 실패하는 인프로세스 ESMTP 서버**입니다. sender의 재시도·에러 분류·레이트리밋을 컨테이너나 네트워크 없이 테스트하기 위해 있고,
나중에 `cmd/chaos-smtp` 가 같은 패키지를 바이너리로 감쌉니다.

## 결정성이 핵심

```go
chaossmtp.Decide(seed, rates, rcpt, attemptNo) // Accept | TempFail | PermFail | Drop
```

`u = SHA256(seed ‖ rcpt ‖ 0x00 ‖ attemptNo)` 의 상위 53비트를 `[0,1)` 로 정규화하고,
`TempFailRate → PermFailRate → DropRate` 순서로 누적 구간에 떨어뜨립니다(나머지는 accept).

**테스트는 서버가 쓴 것과 같은 함수를 호출해 기대값을 계산합니다.** 스냅샷을 저장하지 않으므로
수신자 수나 재시도 정책을 바꿔도 기대값이 자동으로 따라옵니다 (`internal/sender` 의 `TestEndToEndChaos`).

`attemptNo` 는 sender가 붙이는 `X-Sendplane-Attempt` 헤더에서 읽습니다. 헤더가 없으면 수신자별로 세어
(`nextAttempt`) 그 클라이언트에게도 순서가 결정적입니다.

## 옵션

| 옵션 | 동작 |
|---|---|
| `TempFailRate` | DATA 끝에서 `451 4.3.0` |
| `PermFailRate` | DATA 끝에서 `550 5.2.0` |
| `DropRate` | **헤더를 다 읽은 직후 연결을 끊음**(DATA 중간). 응답도 없고 기록도 남지 않음 |
| `RateLimitAfter` | 한 연결에서 N통을 받은 뒤 `MAIL` 에 `421 4.7.0` |
| `Latency` | DATA 종료 응답 전 sleep |
| `Seed` | 실패 수열 선택 |
| `Username`/`Password`/`RequireAuth` | AUTH PLAIN·LOGIN, 미인증 `MAIL` 은 `530 5.7.0` |
| `KeepBodies` | 본문 보관(기본 off: 5,000통 테스트에 본문은 필요 없음) |
| `BounceHook` | 수락된 메시지마다 동기 호출 → e2e에서 DSN 합성용 |

`Messages()` 는 수락된 메시지(envelope from, rcpt, 헤더, 크기, attempt), `Stats()` 는 카운터 스냅샷입니다.

**drop이 기록 전에 일어나는 것이 중요합니다.** 그래서 "수락된 메시지 중 Message-ID 중복 없음" 을
그대로 단언할 수 있고, 중복이 보이면 그건 진짜 이중 발송입니다.

## 프로토콜 범위

EHLO/HELO, AUTH PLAIN·LOGIN, MAIL, RCPT, DATA, RSET, NOOP, VRFY(502), QUIT.
EHLO 응답: `PIPELINING`, `8BITMIME`, `SIZE`, (설정 시) `AUTH PLAIN LOGIN`, `ENHANCEDSTATUSCODES`.
`MAIL FROM:` 은 꺾쇠 유무를 모두 받습니다 — go-mail의 smtp 클라이언트가 꺾쇠 없이 보냅니다.

**STARTTLS는 아직 없습니다**(광고하지 않고, 굳이 보내면 `454 4.7.0`). sender의 TLS 경로는
전송 실패 → `auth` 분류로 검증되고(`classify_test.go`), 테스트용 인증서 체인을 만드는 비용이
지금 얻는 것보다 큽니다. 붙일 때는 `Start` 에 `TLSConfig` 를 받아 `STARTTLS` 를 광고하고
`tls.Server` 로 교체한 뒤 세션 상태를 초기화하면 됩니다.

## 처리량

`TestThroughput` 이 8개 연결로 2,000통을 보내 **1,000 msg/s 이상**을 확인합니다(로컬 실측 수만 msg/s).
메시지당 비용은 SHA-256 한 번과 헤더 파싱뿐입니다.
