# ADR-0016: 프로브 수신 채널 — IMAP + 전역 inbound webhook

- 상태: accepted
- 날짜: 2026-09-22
- 관련: [ADR-0012](0012-loopback-health-probe.md) (루프백 프로브), [ADR-0015](0015-mailbox-health-separate.md) (메일박스 헬스),
  [architecture.md §11](../architecture.md), `internal/probe/inbound/`

## 맥락

루프백 프로브(ADR-0012)는 지금까지 수신 경로가 하나뿐이었습니다. `ProbeMailbox` 는 IMAP 계정이고,
control 리더의 `probe-collect` 루프가 1분마다 로그인해 `X-Sendplane-Probe` 헤더로 검색합니다.
**받은편지함과 스팸함을 구분할 수 있는 유일한 채널**이라는 큰 장점이 있습니다.

그런데 요즘 쓰는 수신 서비스 중에는 IMAP을 아예 제공하지 않고 **받은 메일을 웹훅으로 POST** 하는 쪽이 있습니다.
이런 주소를 프로브 메일박스로 쓰려면 sendplane이 폴링하는 대신 **받아야** 합니다.

제약 두 가지가 결정을 좁힙니다.

1. **웹훅 엔드포인트는 URL입니다.** 프로바이더 쪽 설정에 URL 하나를 적어 두는 것이고, 테넌트마다 다른 URL을 주려면
   테넌트마다 호스트네임이 필요합니다. 즉 이 설정은 태생적으로 **프로세스 전역**입니다.
2. **원본 RFC 822 메시지가 없습니다.** 이런 서비스는 파싱된 헤더 맵과 본문을 주고,
   **도착 폴더를 알 방법이 없습니다** — "배달됐다"는 알려줘도 수신자 클라이언트가 그걸 어디에 분류했는지는 모릅니다.

### 왜 특정 서비스의 포맷을 그대로 쓰지 않았는가 (conduit.email 기각)

처음 검토한 대상은 [conduit.email](https://conduit.email/) 이었고, 실제로 그 포맷으로 구현까지 했다가 **되돌렸습니다.**

이유는 하나이고 치명적입니다. **conduit의 웹훅 페이로드에는 `Authentication-Results` 헤더가 없습니다.**
§11.4 판정은 전부 이 헤더에서 나옵니다 — SPF/DKIM/DMARC 결과가 여기 말고는 어디에도 없습니다.
이게 없으면 프로브가 돌아와도 판정은 영구히 "신뢰 가능한 `Authentication-Results` 없음" yellow이고,
그건 프로브를 돌리지 않은 것과 정보량이 같습니다. **수신 채널로 성립하지 않습니다.**

여기서 배운 것이 설계를 바꿨습니다: **판정에 필요한 것은 프로바이더가 무엇을 주기로 했는지에 달려 있고,
그건 우리가 통제할 수 없습니다.** 그래서 남의 포맷에 맞추는 대신, **판정이 읽는 것만 담은 최소 바디를
sendplane이 정의**하기로 했습니다. JSON을 POST할 수 있는 것이면 무엇이든 — 수신 서비스의 웹훅,
Cloudflare Email Worker, MX 위의 스크립트 스무 줄 — 이 바디를 만들 수 있고, 그 과정에서
`Authentication-Results` 를 보존하는 것이 **명시적 계약**이 됩니다.

서명 방식만은 conduit의 webhook verifier(<https://conduit-v2.mintlify.app/webhook-verifier>)를 그대로 가져왔습니다.
새로 만들 이유가 없고, 세 가지가 좋습니다: 서명 문자열 안의 타임스탬프가 **상태 없이** 리플레이를 막고,
`v1=` 를 여러 개 실을 수 있어 **시크릿 로테이션이 무중단**이며, 와이어 포맷이 `openssl dgst` 한 줄로 만들어질 만큼 단순합니다.

## 결정

**프로브 수신 채널을 두 개로 만듭니다: IMAP과 전역 inbound webhook.**

1. `store.ProbeMailbox.Kind` 를 추가합니다(`imap` | `webhook`, 빈 값 = `imap`).
   `webhook` 이면 `Address` / `AuthServID` / `Enabled` / `Health` 만 의미가 있고,
   `Host/Port/TLS/Username/Password/InboxFolder/SpamFolder` 는 **API가 422로 거부합니다.**
   무시하지 않고 거부하는 이유: 아무도 다이얼하지 않는 호스트와 비밀번호가 들어 있는 행은
   나중에 운영자가 "프로브가 여기로 로그인한다"고 읽게 됩니다.

2. 수신 포맷은 `internal/probe/inbound.Provider` 인터페이스입니다.
   `Name() / Verify(r, body, secrets) / Parse(body)` 셋뿐입니다.
   프로바이더 패키지가 `init()` 에서 레지스트리에 자기를 등록하고, 루트(`probe.go`)가 side-effect import 합니다.
   **HTTP 상태 코드는 인터페이스에 없습니다** — 200 accepted / 200 ignored / 400 malformed /
   401 bad signature / 503 store error 매핑은 핸들러 한 곳(`internal/api/probeinbound.go`)에 있습니다.
   포맷이 상태 코드를 고르게 두면 언젠가 하나가 "우리 메일 아님"에 404를 답하고, 발신 측은 그 메일을 영원히 재전송합니다.

3. **기본 제공 포맷은 `sendplane`** (`internal/probe/inbound/sendplanehook`)입니다.

   ```json
   {"from": "...", "to": ["..."], "headers": {"Header-Name": ["value", ...]}, "text": "..."}
   ```

   `from` 만 필수이고(메일이 아닌 바디를 200으로 삼키지 않기 위해), 모르는 필드는 무시하며,
   헤더 키는 `textproto.CanonicalMIMEHeaderKey` 로 정규화하고 **같은 이름 안의 값 순서는 보존**합니다.
   바디 상한 1 MiB. 포워더는 `Authentication-Results` · `Received` · `X-Sendplane-Probe` 를 **반드시 보존해야 합니다.**

4. **서명은 `X-Sendplane-Signature: t=<unix>,v1=<hex>[,v1=...]`** 이고
   `v1 = hex(HMAC-SHA256(secret, "<t>" + "." + raw body))` 입니다(`internal/probe/inbound/sigv1`).
   검증은 원본 바디로, 상수 시간 비교로, **어느 `v1` 이든 어느 설정된 시크릿이든 하나만 맞으면** 통과입니다
   (양쪽 모두 로테이션 가능). `|now - t|` 가 tolerance(기본 300s)를 넘으면 리플레이로 거부합니다.
   시크릿은 접두사 포함 **원문 그대로** 키로 씁니다.

5. 설정은 `host.ProbeConfig.Webhooks []ProbeWebhook{Provider, Secrets, Path, Tolerance}`,
   YAML로는 `probe.webhooks` 입니다(`secret` 은 1개짜리 `secrets` 의 설탕). **전역이고 모든 테넌트에 동작합니다.**
   기본 경로는 `/probe/inbound/<provider>` 이고, 트래킹 라우트와 같은 방식으로 마운트됩니다 —
   인증 없음, IP당 레이트 리밋, 1 MiB 바디 상한.
   등록되지 않은 프로바이더 이름은 **기동 시점에** 등록된 이름 목록과 함께 거부합니다.

6. **테넌트는 프로브 토큰에서 나옵니다.** `X-Sendplane-Probe` 토큰 형식을
   `<run>/<mac>` 에서 `<tenant>/<run>/<mac>` 으로 바꿨습니다(MAC은 둘 다 덮습니다).
   트래킹 토큰이 `TenantID` 를 싣는 것과 같은 이유입니다: 전역 미인증 엔드포인트에서 테넌트 해석은
   `Provider.ForTenant` 한 번이어야지, 모든 테넌트의 pending run을 훑는 일이 되어서는 안 됩니다.
   MAC 검증은 그 테넌트의 run을 읽은 **뒤에** 합니다.

7. **폴더 `unknown` 은 중립입니다.** `verdict.go` 에서 "받은편지함이 아님"은 이제
   `FolderOther` — 메일박스가 폴더 매핑을 설정해 뒀는데 거기로 안 들어온 경우 — 에만 걸립니다.
   웹훅이 폴더를 못 본다는 사실을 "스팸함에 갔다"로 읽으면 멀쩡한 sender가 영원히 yellow가 됩니다.

8. **판정 코드는 채널이 공유합니다.** `probe.Evidence{Mailbox, Headers, Folder, ReceivedAt, RawHeaders}` 를
   만들고 `Runner.CompleteRun(ctx, st, run, ev, now)` 가 §11.4 판정 · DNS 진단 계층 · `ProbeRun` 컬럼을
   전부 처리합니다. IMAP 수집기는 원본 메시지에서, 웹훅은 헤더 맵(`HeadersFromMap`)에서 `Evidence` 를 채울 뿐입니다.
   sender 요약 갱신은 `RefreshSenderHealth` 로 분리했습니다 — `CollectWith` 는 메일박스를 다 끝낸 뒤
   sender마다 한 번만 부르기 때문입니다(중간값마다 `sender.health_changed` 가 나가면 안 됩니다).

9. **멱등성 두 겹.** 1차는 `ProbeRun.Pending` (스토어에 있음, 이것이 진짜 계약입니다),
   2차는 `Message.ProviderID` 의 작은 LRU입니다. `sendplane` 포맷에는 트랜잭션 ID가 없으므로
   **원본 바디의 sha256**(`inbound.BodyID`)이 그 자리를 대신합니다 — 재전송은 같은 바이트이고,
   서로 다른 프로브 메일은 최소한 토큰이 다릅니다. 서명은 타임스탬프 때문에 재전송마다 달라지므로 키로 쓸 수 없습니다.
   완료된 run의 재전송은 `200 accepted` 이고 아무것도 건드리지 않습니다.

10. **웹훅 메일박스의 헬스**(ADR-0015)는 로그인이 없으므로 다르게 씁니다:
    프로브가 도착할 때마다 `ok`, `probe-collect` 가 타임아웃으로 run을 닫을 때
    stage `webhook`, reason `no probe received within timeout` 으로 `error`.
    `mailbox-check` 루프는 이 kind를 건너뜁니다 — 확인할 자격증명이 없습니다.

## 기각한 대안

**conduit.email 포맷을 기본 제공자로 삼기.** 위(맥락)에 적은 이유로 기각했습니다: 페이로드에
`Authentication-Results` 가 없어 판정 근거가 0입니다. 구현은 되돌렸고, 서명 알고리즘만 남겼습니다.

**테넌트별 웹훅 시크릿 / 테넌트별 엔드포인트.** 멀티테넌시의 기본값(ADR-0006)을 생각하면 가장 먼저 떠오르는 모양이고,
실제로 거부한 이유는 셋입니다.

- 엔드포인트는 URL입니다. 테넌트를 URL에 넣으면(`/probe/inbound/sendplane/{tenant}`) 그 URL은
  **테넌트 ID가 공개되는 미인증 경로**가 되고, 토큰 MAC이 이미 하는 일을 경로가 한 번 더 하는 셈입니다.
- 시크릿을 테넌트 행에 두면 서명을 검증하기 *전에* 테넌트를 알아야 하는데, 테넌트를 아는 유일한 방법은
  아직 검증하지 않은 바디를 파싱하는 것입니다. 순서가 뒤집힙니다.
- 사용자 요구사항이 명시적으로 "설정 파일을 통해 모든 tenant에 global하게" 입니다.

전역 시크릿의 실질적 비용은 "한 테넌트의 프로바이더 설정이 다른 테넌트의 프로브에 영향을 준다"가 아니라
"시크릿 로테이션이 배포 단위"라는 것뿐이고, 그마저 `secrets` 목록으로 무중단입니다.
판정을 위조하려면 여전히 **프로브 HMAC 키**가 필요합니다 —
서명이 맞는 요청이라도 토큰 MAC이 틀리면 `ignored` 입니다. 방어선이 두 개인 이유가 이것입니다.

**원본 RFC 822 메시지를 요구하기.** `Parse` 가 `Raw []byte` 를 돌려주게 하면 기존 파서를 한 줄도 안 고쳐도 됩니다.
하지만 파싱된 메일을 주는 서비스는 원본을 주지 않고, 요구하면 이 채널 자체가 성립하지 않습니다. 실제로 §11.4 판정이 읽는 것은
`Authentication-Results` / `Received` / `Date` 뿐이고 전부 헤더 맵에 있습니다.
대신 `Headers` 의 **값 순서**(같은 이름 안에서)를 계약으로 못 박았습니다 — `Received` 체인을 아래에서 위로 읽고
"첫 `Authentication-Results`"를 신뢰 판단에 쓰기 때문에, 그 순서만 보존되면 맵으로 충분합니다.

**폴더를 `inbox` 로 가정하기.** 프로바이더가 배달했다면 받은편지함일 것이다 — 편하지만 거짓입니다.
스팸 분류는 프로브가 존재하는 이유의 절반이고, 모르는 것을 안다고 기록하면 그 절반이 조용히 사라집니다.
`unknown` 으로 남기고 중립 처리하는 쪽이 정직합니다. **IMAP 메일박스를 최소 하나는 함께 등록하라**는 권고가
이 한계에 대한 답입니다(§11.1의 "여러 개 등록 권장"이 그대로 적용됩니다).

**서명 없이 URL의 추측 불가능성에 기대기.** 토큰 MAC이 이미 있으니 서명은 중복 아니냐는 반론이 가능합니다.
아닙니다. 서명이 없으면 **아무나** 이 엔드포인트에 요청을 보낼 수 있고, 유효한 프로브 메일을 한 번이라도 본 쪽은
(수신 측 MTA, 메일박스 주인, 중간의 누구든) 그 토큰을 그대로 재사용해 판정을 만들 수 있습니다.
서명은 "이 요청이 우리가 설정한 포워더에서 왔다", 토큰 MAC은 "이 메일이 우리가 보낸 프로브다"를 말합니다.

**생성된 strict 핸들러로 라우팅하기.** 스펙에 넣고 `oapi-codegen` 이 만들어 주는 핸들러를 쓰는 쪽이 균일합니다.
두 가지가 막습니다: 라우트는 **설정된** 프로바이더마다, 호스트가 바꿀 수 있는 경로에 마운트되고,
핸들러는 **요청 자체**(서명이 덮는 바로 그 바이트와 그 서명 헤더)가 필요합니다.
그래서 스펙에는 문서로 남기되(`Inbound (public)`, `security: []`, `x-sendplane-action: none`)
`exclude-operation-ids` 로 코드 생성에서 빼고 손으로 마운트합니다.

## 결과

- 포맷 추가 비용: 인터페이스 3개 메서드 + `init()` + 루트 import 한 줄(`internal/probe/README.md` 에 레시피).
- 같은 `t=/v1=` 서명을 쓰는 프로바이더는 `inbound/sigv1` 을 그대로 재사용합니다.
- 토큰 형식이 바뀌었습니다(`<tenant>/<run>/<mac>`). 아직 출시 전이라 마이그레이션은 없고,
  진행 중이던 run은 다음 트리거에서 정상화됩니다.
- 웹훅 메일박스만 등록한 테넌트는 **스팸 분류를 볼 수 없습니다.** 콘솔은 이 한계를 표시해야 합니다.
- 포워딩 주체가 `Authentication-Results` 를 붙이지/보존하지 않으면 판정은 yellow에 고정됩니다.
  conduit을 기각한 바로 그 이유이고, 그래서 이 요구사항은 README·OpenAPI·`Message.Headers` 주석 세 곳에 적혀 있습니다.
- 재검토 조건: 프로바이더가 원본 메시지나 폴더 정보를 주기 시작하면 `Evidence.Folder` 를 채우면 되고,
  판정 코드는 손댈 것이 없습니다.
