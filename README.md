# sendplane

sendplane은 listmonk/Keila 계열의 임베더블 메일 발송 엔진입니다. Go 라이브러리(`sendplane.New`)로 호스트 애플리케이션에
임베딩하거나, 역할별(`control`/`sender`/`bounce`)로 나눠 배포할 수 있는 참조 바이너리로도 제공됩니다.

- 설계 문서: [docs/architecture.md](docs/architecture.md)
- 결정 기록(ADR): [docs/adr/README.md](docs/adr/README.md)
- 구현 로드맵: [docs/roadmap.md](docs/roadmap.md)
- 기여 가이드: [docs/CONTRIBUTING.md](docs/CONTRIBUTING.md)

## 개발 환경 빠른 시작

```sh
make dev-up              # deploy/dev/docker-compose.yml (postgres:55441, mongo:55442)
cp .env.example .env     # SENDPLANE_TEST_* DSN 설정
make test
```

`make dev-down` 으로 로컬 의존성을 정리합니다. `make ci` 는 CI와 동일한 fmt/vet/lint/test 체크를 로컬에서 실행합니다.

운영 콘솔(Vue, `web/apps/console`)까지 넣은 참조 바이너리를 실행하려면:

```sh
make web-install     # 최초 1회
make console-sync    # web/apps/console/dist 를 빌드해 cmd/sendplane/console/dist 로 복사(ADR-0010)
go build -o sendplane ./cmd/sendplane
cp cmd/sendplane/config.example.yaml config.yaml   # 필요한 값 채우기
./sendplane --config=config.yaml --roles=control,sender,bounce
```

`http://localhost:8080/console/` 에서 콘솔을 엽니다. `console-sync` 없이 빌드해도
`go build`는 되지만(커밋된 플레이스홀더 덕분) 콘솔은 "not built" 페이지만 보입니다.
자세한 내용은 [cmd/sendplane/README.md](cmd/sendplane/README.md#운영-콘솔-cmdsendplaneconsole)를
참고하세요.

## 상태: 개발 초기

아직 공개 API가 안정화되지 않았습니다. 구현 순서는 [docs/roadmap.md](docs/roadmap.md)의 Phase 0부터 진행됩니다.
