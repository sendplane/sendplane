# internal/mailbox

바운스 폴러(§10)와 루프백 프로브(§11.1)가 공유하는 **IMAP/POP3 수신 클라이언트**입니다.
설계 근거는 [architecture.md §10, §11.1](../../docs/architecture.md), [ADR-0012](../../docs/adr/0012-loopback-health-probe.md).

## 표면

```go
type Message struct { ID string; Raw []byte; Received time.Time }

type Client interface {
    Fetch(ctx, max int) ([]Message, error)   // 아직 처리 안 한 메일, 오래된 것부터
    Ack(ctx, ids []string, action Action) error
    Close() error
}

// 선택 인터페이스 (IMAP만 구현)
type HeaderSearcher interface { FetchByHeader(ctx, name, value string) ([]Message, error) }
type Idler         interface { Idle(ctx, timeout time.Duration) error }

func Dial(ctx, cfg Config, cipher host.SecretCipher) (Client, error)
```

`Config`는 스토어 모델(바운스 메일박스 행, `store.ProbeMailbox`)을 **어댑트한 모양**일 뿐이고,
이 패키지는 설정을 어디에 저장하는지 관여하지 않습니다. `Password`는 스토어에 들어 있는 그대로의
암호문이고 `Dial`이 `host.SecretCipher`로 풀어 씁니다(`cipher == nil`이면 평문 취급 — 테스트와,
비밀을 sendplane 밖에서 관리하는 호스트용).

## AfterProcess 정책

| 값 | IMAP | POP3 |
|---|---|---|
| `keep`(기본) | `\Seen`만 붙임 → 다음 `Fetch`에서 제외 | **아무것도 안 함 → 다음 폴링에 다시 옴** |
| `delete` | `\Seen`+`\Deleted` → `UID EXPUNGE`(UIDPLUS 없으면 `EXPUNGE`) | `DELE`, 실제 삭제는 `Close()`의 `QUIT` 시점 |
| `move:<폴더>` | `\Seen` 후 `UID MOVE`(MOVE 없으면 COPY+DELETE+EXPUNGE) | **불가** (`ErrNoFolders`) |

`Fetch`는 IMAP에서 `NOT SEEN NOT DELETED`를 검색합니다. 즉 **`\Seen`을 붙이는 주체는 `Ack`뿐**이고,
처리에 실패해서 ack하지 않은 메일은 다음 패스에 다시 옵니다. POP3에는 그런 플래그가 없으므로
`keep` + POP3 조합은 매번 전체를 다시 내려받습니다 — 소비자가 멱등이어야 합니다
(바운스 프로세서는 `MarkBounced`가 한 번만 전이하므로 멱등입니다).

## 라이브러리 선택

- **IMAP: `github.com/emersion/go-imap/v2`(beta.8) + `imapclient`.** IDLE, UIDPLUS, MOVE, SEARCH를 다 갖고 있고
  테스트용 `imapserver/imapmemserver`가 같이 옵니다(이 패키지 테스트가 실제로 그 서버를 띄웁니다).
  go-imap 클라이언트는 `context`를 받지 않으므로, 명령마다 **커넥션 데드라인**(ctx 데드라인과 `Config.Timeout` 중 이른 쪽)을
  걸고 끝나면 해제합니다. ctx를 넘긴 명령은 커넥션을 죽이고, 그건 폴러의 백오프·재다이얼이 처리합니다.
- **POP3: 직접 구현(`pop3.go`, 270줄).** `knadh/go-pop3`는 STARTTLS가 없고, `context`/데드라인을 못 받고,
  `tls.Config`를 넘길 수 없습니다(`InsecureSkipVerify`만). 사설 CA를 쓰는 메일박스에서 바로 막히는 지점이라
  RFC 1939 + STLS(RFC 2595)의 필요한 명령(`UIDL/RETR/DELE/STLS/QUIT`)만 직접 씁니다. 의존성도 하나 줄어듭니다.

## 자격증명 테스트 (`Test`)

`Test(ctx, cfg, cipher) TestResult` 는 폴러가 하는 그대로 — 다이얼 → TLS → 로그인 → 폴더 선택 → 로그아웃 —
해 보고 **어디까지 갔는지**를 돌려줍니다. 20초 안에 끝나고, **원격 실패는 절대 Go error가 아닙니다**:
"비밀번호가 틀렸다"는 질문의 답이지 질문의 실패가 아니기 때문입니다.

```go
res := mailbox.Test(ctx, cfg, secrets)
// TestResult{OK, Stage, Error, Latency, Folders{name:{Exists,Messages}}, Server}
```

| Stage | 뜻 |
|---|---|
| `config` | 보내 보지도 못함 — 프로토콜/호스트가 없거나 비밀번호 복호화 실패 |
| `dial` | TCP 연결 실패, 또는 그리팅을 못 받음 |
| `tls` | 암시적 TLS 핸드셰이크 / STARTTLS(`STLS`) 실패 |
| `auth` | **서버가 로그인을 거절함** (IMAP 태그드 `NO`/`BAD`, POP3 `-ERR`) |
| `folder` | 로그인은 됐는데 `Folder`나 `ExtraFolders` 중 하나를 `EXAMINE` 할 수 없음 |
| `ok` | 전부 통과 |

`auth`를 네트워크 실패와 구분하는 게 핵심입니다 — 고칠 사람이 다릅니다. IMAP은 `*imap.Error`,
POP3는 이 패키지의 `*ServerError`로 "서버가 거절"을 판별합니다. 폴더는 `EXAMINE`(읽기 전용)으로 열어서
자격증명 점검이 메일박스 상태를 건드리지 않게 합니다. POP3는 폴더가 없으므로 `STAT` 하나가 전부이고,
결과는 설정된 폴더 이름 아래에 담깁니다(호출자가 두 프로토콜을 같은 모양으로 렌더할 수 있게).

`Config.ExtraFolders` 는 `Test` 전용입니다: 프로브 메일박스의 스팸 폴더처럼 `Dial` 이 절대 선택하지 않는
(IMAP 커넥션은 선택된 메일박스가 하나뿐) 폴더를 같이 확인하기 위한 것입니다.

### `Dial` 의 단계 태그

`Dial` 실패도 같은 단계를 답니다: 에러는 `*DialError` 로 감싸여 있고 `StageOf(err)` 로 꺼냅니다
(단계가 없는 에러는 `dial` 로 봅니다). 바운스 폴러와 프로브 수집 루프는 `Test` 를 부르지 않고
`Dial` 실패만 보기 때문에, 이게 있어야 둘도 수동 테스트와 **같은** `store.MailboxHealth` 를 기록합니다.
`*DialError` 는 `MailboxStage() string` 도 구현하는데, 이 패키지를 import 하지 않는
`internal/probe` 가 한-메서드 인터페이스로 단계를 읽기 위한 것입니다.

## Fake

`mailbox.Fake`는 인메모리 구현입니다. 동시 사용이 안전해서 **두 폴러가 같은 메일박스를 두고 경쟁하는 테스트**를
그대로 쓸 수 있습니다(`internal/bounce/poller_test.go`가 그렇게 씁니다).

```go
f := mailbox.NewFake(raw1, raw2)
f.Fetch(ctx, 50)                 // 아직 ack 안 한 것만
f.Ack(ctx, []string{"1"}, mailbox.ActionDelete)
f.Acks()        // 기록된 ack (정책 검증용)
f.Remaining()   // 남은 메일 수
f.FetchErr = errors.New("...")   // 실패 주입
```

## 테스트

```
go test -race ./internal/mailbox/...
```

IMAP은 `imapmemserver`를, POP3는 이 패키지 테스트 안의 스크립트 서버를 루프백에 띄웁니다. 네트워크는 필요 없습니다.
