# internal/dnscheck

발신 도메인의 DNS 레코드 **진단** 계층입니다.
설계 근거는 [architecture.md §11.3](../../docs/architecture.md), [ADR-0012](../../docs/adr/0012-loopback-health-probe.md).

**이 패키지는 헬스 상태를 결정하지 않습니다.** 1차 근거는 루프백 프로브([`internal/probe`](../probe))이고,
여기는 "왜 실패했는가"를 설명합니다. 레코드가 완벽해도 정렬 실패·스팸 분류로 메일은 얼마든지 깨집니다.

## 검사

| 메서드 | 보는 것 | red | yellow |
|---|---|---|---|
| `SPF(ctx, domain, observedIP)` | TXT 존재·구문, DNS 조회 ≤10(RFC 7208 4.6.4), 관측 IP 인가 여부 | 레코드 없음 / permerror / 조회 한도 초과 / `-all`로 fail | softfail(`~all`) · neutral(`?all`) · `all` 없음 · temperror |
| `DKIM(ctx, domain, selector, key)` | `{selector}._domainkey` TXT, `v=`/`k=`/`p=` 파싱, 저장된 개인키에서 유도한 공개키와 비교 | 레코드 없음 / `p=` 없음·빈 값(폐기) / base64·키 파싱 실패 / **공개키 불일치** | `t=y`(테스트 모드), RSA 1024비트 미만 |
| `DMARC(ctx, domain)` | `_dmarc` TXT, `p=`, `rua`, `adkim`/`aspf`(기본 `r`) | 레코드 없음 / 여러 개 / `p=` 없음·미지정 값 | `p=none`, `rua` 없음 |
| `MX(ctx, domain)` | return-path 도메인의 MX | MX 없음, null MX(RFC 7505) | 조회 실패 |
| `PTR(ctx, ip)` | 역방향 + 정방향 일치(FCrDNS) | — | PTR 없음, FCrDNS 불일치 |

`RunAll(ctx, Inputs) Report` 는 입력이 있는 검사만 돌리고 최악 값을 `Report.Status` 로 냅니다.
입력이 없는 검사(선택자 없음, 관측 IP 없음)는 **nil이지 red가 아닙니다** — 물어볼 대상이 없는 질문입니다.

`observedIP` 는 프로브의 `Received` 헤더에서 옵니다(§11.3: 별도 자기 IP 탐지가 필요 없음).
nil이면 SPF는 구문·조회 한도만 보고 PTR은 건너뜁니다.

## 리졸버

```go
r, _ := dnscheck.SystemResolver(5 * time.Second)   // /etc/resolv.conf
r, _ = dnscheck.NewResolver([]string{"1.1.1.1", "8.8.8.8:53"}, 5*time.Second)
c := dnscheck.New(r)
```

`miekg/dns` 로 **지정 네임서버에 직접** 질의합니다. 호스트 스텁 리졸버를 쓰지 않는 이유는
"세상이 보는 레코드"를 알아야 하는 검사가 로컬 캐시나 `/etc/hosts` 로 답해지면 안 되기 때문입니다.
UDP로 묻고 truncated면 TCP로 재시도하며, SERVFAIL/REFUSED는 다음 서버로 넘어갑니다.
NXDOMAIN과 "해당 타입 레코드 없음"은 둘 다 `ErrNoRecord` 입니다 — 헬스 체크에는 같은 사실입니다.

`Resolver` 는 인터페이스이고, 테스트는 전부 `zone` 테이블 기반 페이크로 돕니다(네트워크 접근 0).

## SPF 평가 범위

RFC 7208 `check_host()` 를 진단에 필요한 만큼만 구현했습니다.

- `include`/`redirect` 재귀, `a`/`mx`/`exists`/`ptr` 조회 예산 소비, `ip4`/`ip6` CIDR 비교, 한정자 → pass/fail/softfail/neutral.
- **매크로(`%{i}` 등)는 확장하지 않습니다.** sendplane이 권장하는 레코드에는 없고, 쓰는 도메인은 결과가 보수적으로 나옵니다.
- `ptr` 메커니즘은 RFC 7208 5.5에서 폐기 권고이므로 **매치하지 않되 조회 예산은 소비**합니다.
- 같은 이름의 TXT는 한 번만 질의하고 예산에는 규칙대로 매번 부과합니다.

## 테스트

```
go test -race ./internal/dnscheck/...
```
