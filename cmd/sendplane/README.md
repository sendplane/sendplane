# cmd/sendplane

sendplane 라이브러리(`sendplane.New`)를 감싼 참조 바이너리입니다. 설정 파일 하나로 API Key/JWT 인증, webhook
이벤트 전달, AES-GCM 비밀 암호화를 구성하고, 같은 바이너리·같은 이미지로 `control`/`sender`/`bounce` 역할을
원하는 조합으로 실행합니다(ADR-0001, docs/architecture.md §2-3).

## 빌드 · 실행

```sh
go build -o sendplane ./cmd/sendplane
cp config.example.yaml config.yaml   # 필요한 값 채우기
./sendplane --config=config.yaml --roles=control,sender,bounce
```

## 플래그 / 환경변수

| 플래그 | 환경변수 | 기본값 | 설명 |
|---|---|---|---|
| `--config` | `SENDPLANE_CONFIG` | `config.yaml` | 설정 파일 경로 |
| `--roles` | `SENDPLANE_ROLES` | `control,sender,bounce` | 실행할 역할(쉼표 구분) |
| `--listen` | - | `:8080` | HTTP 서버 주소 |
| `--migrate` | - | `false` | 마이그레이션만 실행하고 종료 |
| `--migrate-on-start` | - | `false` | 역할을 시작하기 전에 마이그레이션 실행 |

HTTP 서버는 어떤 역할 조합이든 항상 기동되고 `/healthz`를 응답합니다(쿠버네티스 프로브용). `control`이
`--roles`에 있으면 `sp.Handler()`(REST API)가 `/`에 마운트되고 `/healthz`는 **그 핸들러가** 스펙대로 JSON으로
답합니다. control이 없는 sender/bounce 전용 파드에서만 평문 `/healthz`를 따로 등록합니다 — 둘 다 등록하면
`net/http` mux가 더 구체적인 `/healthz`를 골라 API 라우트를 가려 버립니다.

## 설정 파일

전체 스키마는 [config.example.yaml](config.example.yaml)을 참고하세요. `${VAR}` 형태의 값은 프로세스
환경변수로 치환된 뒤 파싱됩니다(`os.ExpandEnv`) — Helm 차트가 Secret의 DSN/키를 ConfigMap 없이 주입하는
방식이 바로 이것입니다.

- `store`: `driver`(postgres|mongo), `dsn`, `mongo_db`
- `secrets.key`: AES-256-GCM용 32바이트, base64. `openssl rand -base64 32`로 생성
- `auth.mode`: `apikey`(해시된 키 목록) · `jwt`(JWKS 검증, 캐시/자동 갱신) · `none`(로컬 개발 전용, 기동 시
  경고 로그)
- `authz.mode`: `allow_all` 또는 `roles`(역할별 glob 패턴을 `host.Action` 문자열에 매칭)
- `events.webhook`: HMAC-SHA256 서명(`X-Sendplane-Signature`) webhook. SSRF 방지를 위해 기본적으로
  사설/루프백 대역으로의 연결을 거부합니다(`allow_private_networks`로 해제)
- `sender`: 레인별 워커 수, claim 배치 크기, 리스 시간 등 (`docs/architecture.md` §8). `lanes.probe`는 최소 1이어야
  루프백 프로브가 실제로 나갑니다
- `bounce`: 메일박스별 lock owner(`worker_id`), 폴링/새로고침 주기, IMAP IDLE 사용 여부 (§10).
  메일박스 자체는 테넌트 리소스입니다: `POST /api/v1/bounce-mailboxes`
- `probe`: 루프백 헬스 체크 (§11). `hmac_key`(base64)가 있으면 켜지고 `enabled`로 명시적으로 덮을 수 있습니다.
  `nameservers`는 DNS 진단 계층이 직접 질의할 리졸버입니다
- `limits`: `host.Limits`에 매핑, 0인 필드는 기본값으로 채워집니다

## cmd/chaos-smtp

`internal/chaossmtp`(결정적으로 실패하는 테스트용 SMTP 서버)를 감싼 얇은 바이너리입니다.
`--tempfail/--permfail/--drop/--latency/--seed/--ratelimit-after`로 실패율을 설정하고,
`--stats-listen`(기본 `:9090`)의 `GET /stats`/`POST /reset`으로 상태를 조회·초기화합니다.
