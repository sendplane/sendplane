# ADR-0009 MJML은 publish 시 컴파일, 블록 편집기는 GrapesJS-MJML

상태: accepted · 2026-09-21 (스파이크 GO: grapesjs 0.23.6 + grapesjs-mjml 1.0.8, BSD-3; 근거는 web/packages/ui/docs/block-editor-spike.md)

## 맥락
블록 위지윅과 HTML 직접 편집 둘 다 필요하다. MJML 컴파일은 Node 기반이고 무겁다(수십~수백 ms). 프론트는 Vue.

## 결정
- 세 가지 본문 모드 `blocks | mjml | html`. blocks는 GrapesJS 프로젝트 JSON과 그것이 내보낸 MJML을 함께 저장.
- publish 시 Layout 슬롯 병합 → `Boostport/mjml-go`(WASM, Node 불필요)로 **1회 컴파일** → MessageVersion에 HTML 저장. 발송 시엔 Liquid만.
- MJML 구조 사이의 Liquid 제어문은 `<mj-raw>` 컨벤션. 블록 편집기가 "조건" 속성으로 이를 생성.
- 편집기는 GrapesJS + grapesjs-mjml을 Vue 컴포넌트로 래핑.

## 기각한 대안
- **수신자별 MJML 컴파일(Keila v0.30 방식, Liquid→MJML)**: 작성 편의는 좋으나 1M 수신자면 수 시간의 CPU. 규모 요구와 양립 불가.
- **listmonk v5의 `@usewaypoint/email-builder-js`**: React 전용. Vue 앱에 React 런타임을 끼우는 비용이 크다.
- **Unlayer 등 상용 에디터**: 라이선스/오프라인 문제.
- **MJML 서비스(Node 사이드카)**: 컴파일이 publish 시에만 있으므로 사이드카까지 필요 없다. mjml-go가 충분하지 않으면 그때 검토.

## 재검토 조건
- 스파이크(roadmap Phase 8)에서 GrapesJS-MJML이 i18n 태그 삽입, Vue 3 + Vite 번들, 라이선스(BSD-3)를 만족하는지 확인. 실패 시 대안: 자체 경량 블록 모델(섹션/컬럼/텍스트/버튼/이미지) → MJML 생성.
