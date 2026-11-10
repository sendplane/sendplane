# deploy/helm/sendplane

sendplane 참조 바이너리(`cmd/sendplane`) 하나를 `control`/`sender`/`bounce` 세 역할로 나눠 배포하는 Helm
차트입니다(docs/architecture.md §14). DB(Postgres/MongoDB)는 차트 밖의 외부 리소스로 가정합니다.

## 설치

```sh
helm install my-sendplane deploy/helm/sendplane \
  --set image.repository=sendplane --set image.tag=dev \
  --set secrets.storeDSN='postgres://...' \
  --set secrets.secretsKey='<base64 32바이트>' \
  --set secrets.webhookSecret='<webhook HMAC 키>'
```

운영 환경에서는 `secrets.existingSecret`으로 직접 관리하는 Secret(sealed-secrets, external-secrets 등)을
가리키는 편을 권장합니다 — `values.yaml`의 평문 필드는 로컬 테스트용입니다.

## 구성 요소

| 리소스 | 역할 |
|---|---|
| `deployment-control` | REST API + 리더 루프(스케줄러/완료판정/보존기간/이벤트 아웃박스). 기본 2 replica + PDB |
| `deployment-sender` | claim/렌더/SMTP 발송 루프. CPU 기준 HPA(기본), 큐 깊이 기반 KEDA 예시는 `templates/hpa-sender.yaml`에 주석 처리 |
| `deployment-bounce` | IMAP/POP3 바운스 폴러. 기본 1 replica(`bounce.enabled=false`로 끌 수 있음) |
| `job-migrate` | `sendplane --migrate`를 실행하는 `pre-install,pre-upgrade` 훅 Job |
| `configmap` | `values.config`를 그대로 렌더링한 `config.yaml`. `${VAR}`는 컨테이너 안에서 바이너리가 직접 치환 |
| `secret` | DSN/암호화 키/webhook 시크릿(`SENDPLANE_STORE_DSN` 등). `secrets.existingSecret`로 대체 가능 |
| `service` / `ingress` | control만 노출. API 호스트와 tracking 도메인 호스트를 같은 서비스로 라우팅 |
| `service-metrics` | `<fullname>-metrics` headless Service. `metrics.enabled`(기본 true)일 때 control/sender/bounce 파드 전부를 `metrics` 포트로 선택 |
| `servicemonitor` | Prometheus Operator CRD 필요, 기본 비활성. `<fullname>-metrics`를 `/metrics`로 스크레이프하고 `app.kubernetes.io/component` 라벨을 시리즈에 붙임 |

`/healthz`는 세 역할 모두 동일하게 응답합니다(쿠버네티스 프로브용 — `cmd/sendplane`은 역할과 무관하게 HTTP
서버를 항상 띄웁니다). REST API(`/`)는 control 파드에만 마운트됩니다.

## 메트릭

- `metrics.enabled`(기본 `true`) / `metrics.port`(기본 `9090`): 세 Deployment가 `--metrics-listen=:<port>`로 Prometheus
  `GET /metrics`를 별도 포트(컨테이너 포트 이름 `metrics`)에 노출합니다. 메인 `http` 포트는 Ingress로 도달 가능하고 메트릭에는
  테넌트 ID가 들어 있으므로 일부러 분리했습니다.
- `metrics.enabled=true`이면 headless Service `<fullname>-metrics`가 만들어집니다. selector에 component 라벨이 없어 세 역할의
  파드를 모두 선택합니다(migrate Job 파드도 selector에는 맞지만 `metrics` 포트가 없어 엔드포인트에서 제외됩니다).
- `serviceMonitor.enabled=true`는 `metrics.enabled=true`를 요구하며, 아니면 렌더링이 실패합니다. `serviceMonitor.path`는 없습니다
  (바이너리는 `/metrics`만 서빙). `interval`/`scrapeTimeout`/`labels`로 조정합니다.
- `sendplane_delivery_queue_depth`는 control 파드만 내보내며 모든 replica가 같은 전역 값을 보고하므로 `sum`이 아니라 `max`로
  집계하세요. `pending`에는 예약/일시중지된 캠페인의 행이 포함됩니다.

## 값 검증

```sh
helm lint deploy/helm/sendplane
helm template deploy/helm/sendplane | kubectl apply --dry-run=client -f -
```
