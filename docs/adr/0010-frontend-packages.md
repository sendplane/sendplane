# ADR-0010 프론트 패키지 분리 (api / ui / console)

상태: accepted · 2026-09-20

## 맥락
전체 사이트 앱뿐 아니라 API 라이브러리와 UI 라이브러리를 별도 사이트가 페이지 단위로 가져다 써야 한다.

## 결정
- pnpm workspace: `@sendplane/api`(OpenAPI 생성 타입 + fetch 클라이언트, 프레임워크 무관), `@sendplane/ui`(Vue 3 컴포넌트와 페이지 컴포넌트), `@sendplane/console`(라우터·인증 어댑터·조립).
- `ui`는 vue-router에 의존하지 않는다. `provide('sendplane', {client, navigate, t})`로 주입받고 링크 이동은 `navigate(to)` 콜백. 라우팅 테이블은 호스트 앱(또는 console)이 소유.
- UI 문자열은 vue-i18n 메시지 번들을 외부 주입 가능(기본 en/ko).
- 무거운 편집기는 dynamic import로 분리 청크.
- 참조 바이너리는 console 빌드를 `embed.FS`로 서빙.

## 기각한 대안
- **단일 앱만 제공하고 iframe 임베드**: 인증/스타일/라우팅 통합이 불편.
- **Web Components로 배포**: Vue 호스트라는 전제가 있으므로 불필요한 추상화.

## 결과
- `openapi.yaml` 변경 → `api` 재생성 → `ui` 타입 오류로 drift가 즉시 드러난다(CI에 생성물 diff 검사).
