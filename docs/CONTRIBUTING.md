# 기여 가이드

## 스토어 적합성(conformance) 테스트

`store` 패키지가 정의하는 리포지터리 인터페이스는 `store/storetest`에 구현체 독립적인 스위트로 검증합니다.
새 스토어 구현(Postgres, MongoDB, 그 외 커스텀 구현)을 추가하거나 수정할 때는 **반드시 `storetest.Run`을 통과해야
합니다**. 테넌트 격리, 페이지네이션, 멱등 삽입/전이 같은 계약은 각 구현이 개별적으로 재검증하지 않고 이 스위트 하나로
보장합니다(자세한 배경은 [ADR-0007](adr/0007-repository-conformance.md) 참고).

## 환경 변수 규칙

DB를 사용하는 conformance 테스트는 아래 두 환경 변수로 대상 DB에 접속하며, **값이 없으면 해당 백엔드 테스트를
스킵**합니다. 로컬 값은 `.env.example`을 참고하세요.

- `SENDPLANE_TEST_POSTGRES_DSN`
- `SENDPLANE_TEST_MONGO_URI`

로컬에서 두 DB를 띄우려면 `make dev-up`(`deploy/dev/docker-compose.yml`)을 사용하세요.

## 푸시 전 체크

```sh
make ci   # fmt-check, vet, lint, test
```

CI(`ci.yml`)도 동일한 체크에 더해 `storetest` 매트릭스(postgres, mongo)를 실행합니다.
