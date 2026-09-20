# ADR-0004 Liquid 템플릿, `{% t %}` i18n 태그, HTML 기본 이스케이프

상태: accepted · 2026-09-21 (사용자 확인: Liquid 채택)

## 맥락
요구: 템플릿에 i18n 키를 적고 로케일별 key-value, 변수 사용, YAML import/export. 렌더는 1M 수신자에 대해 sender에서 수행되며 HTML 인젝션에 안전해야 한다. 에디터 미리보기도 필요하다.

## 결정
- 표현식 언어는 **Liquid** (Go: `osteele/liquid`, 브라우저 근사 미리보기: `liquidjs`). `include/render/layout` 태그 비활성, strict variables는 미리보기에서 on, 발송에서는 off+경고 이벤트.
- i18n은 커스텀 태그 `{% t "key" %}` / `{% t "key" name: recipient.name %}` 와 필터 `{{ "key" | t }}`. 번역값도 Liquid로 파싱(캐시)되어 변수 삽입 가능.
- 폴백: recipient.locale → 언어 → campaign.default_locale → template.default_locale → 키 문자열.
- **HTML 파트는 바인딩 문자열을 사전 이스케이프**한다. 신뢰 HTML은 `{"$html": "..."}` 값으로만 통과. subject/text는 이스케이프 없음.
- MJML 컴파일은 publish 시 1회, Liquid는 발송 시 수신자별(ADR-0009).

## 기각한 대안
- **단일 중괄호 커스텀 문법 `{user.name}`**: 파서가 작고 안전하지만 (a) 조건/반복/필터가 없어 실제 마케팅 템플릿에서 금방 한계 (b) CSS/JS/MJML 속성의 `{`와 충돌 위험 (c) 양쪽(Go/JS) 구현을 직접 유지해야 함. Liquid는 Keila/Shopify로 사용자에게 친숙하고 양쪽 구현이 이미 있다.
- **Go `text/template`(listmonk)**: 마케터에게 낯설고 브라우저 구현이 없다.
- **Mustache**: 로직리스라 안전하나 필터(날짜/숫자 포맷)가 없어 i18n 실사용에 불리.
- **자동 이스케이프 없이 `| escape` 필터 권장**: 기본이 안전하지 않다. 이스케이프 누락 한 번이 인젝션.

## 결과
- 요구서의 `{user.name}`은 `{{ recipient.name }}`/`{{ vars.user.name }}`로 매핑된다. 이 결정이 뒤집히면 `internal/render`만 영향.
- 번역값에 Liquid를 허용하므로 번역자가 태그를 깨뜨릴 수 있다 → import 시 파싱 검증.
