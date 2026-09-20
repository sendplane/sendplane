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
| `servicemonitor` | Prometheus Operator CRD 필요, 기본 비활성 |

`/healthz`는 세 역할 모두 동일하게 응답합니다(쿠버네티스 프로브용 — `cmd/sendplane`은 역할과 무관하게 HTTP
서버를 항상 띄웁니다). REST API(`/`)는 control 파드에만 마운트됩니다.

## 값 검증

```sh
helm lint deploy/helm/sendplane
helm template deploy/helm/sendplane | kubectl apply --dry-run=client -f -
```
