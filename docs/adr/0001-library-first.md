# ADR-0001 라이브러리 우선 + 참조 바이너리

상태: accepted · 2026-09-20

## 맥락
sendplane은 다른 사이트/프로젝트에 통합되어야 하고, 계정·권한·수신거부는 호스트가 구현한다. 동시에 k8s 차트로 단독 배포도 되어야 한다.
호스트 로직을 주입하는 가장 표현력 있는 방법은 Go 인터페이스이지만, 호스트가 Go가 아닐 수도 있다.

## 결정
- 루트 패키지 `sendplane`이 `New(Options)`로 `Authenticator`, `Authorizer`, `TenantResolver`, `Hooks`를 받는 **라이브러리**가 1급 산출물이다.
- `cmd/sendplane`은 이 라이브러리를 설정 파일 기반 구현체(API Key/JWT 인증, webhook 이벤트, URL 템플릿 수신거부)로 감싼 **참조 바이너리**이며, Go 없이 통합하려는 호스트와 helm 배포가 쓴다.
- control / sender / bounce 역할은 같은 바이너리의 `--roles` 플래그. 호스트가 Go 훅을 쓰면 호스트가 자기 `cmd/`에서 세 역할을 링크한다(훅은 sender에서도 실행되므로).
- 공개 패키지는 `sendplane`과 `store`만. 나머지는 `internal/`.

## 기각한 대안
- **바이너리 전용 + 모든 확장을 webhook으로**: 비-Go 호스트에는 충분하지만, Go 호스트가 인증을 in-process로 하고 싶을 때 네트워크 홉과 지연이 생긴다. 참조 바이너리가 이 모델을 그대로 제공하므로 잃는 것이 없다.
- **플러그인(go-plugin/gRPC)**: 운영 복잡도 대비 이득 없음.

## 결과
- 훅이 없어도 동작해야 하므로 모든 훅은 선택적이고 기본 동작이 정의되어 있다.
- 공개 표면이 좁아 semver 유지가 쉽다. `internal/` 안은 자유롭게 바꾼다.
