# 블록 편집기 스파이크: GrapesJS + grapesjs-mjml

- 일자: 2026-09-21 · 대상: ADR-0009, architecture §6.3 / §13 (로드맵 8단계)
- **결론: GO.** 아래 (a)~(f) 전부 통과. 다만 **Liquid 이스케이프 후처리**와
  **`data-*` 속성 금지** 두 가지는 반드시 지켜야 한다.

## 버전 · 라이선스

| 패키지          | 버전                | 라이선스     | 비고                           |
| --------------- | ------------------- | ------------ | ------------------------------ |
| `grapesjs`      | 0.23.6 (2026-08-25) | BSD-3-Clause | 활발히 유지 중                 |
| `grapesjs-mjml` | 1.0.8 (2026-03-13)  | BSD-3-Clause | `mjml-browser` 4.x를 끌고 온다 |

ADR-0009이 요구한 BSD-3 조건을 둘 다 만족한다. 상용/오프라인 제약 없음.

## 검증 방법

happy-dom(vitest 환경)에서도 GrapesJS는 부팅되지만 **happy-dom의
`Attr.nodeName`이 `''`, `Attr.nodeValue`가 `null`을 돌려준다**(20.14.5).
GrapesJS 파서가 바로 그 두 값을 읽기 때문에 문서의 모든 속성이 빈 키 하나로
뭉개진다 — 즉 happy-dom에서의 export 검증은 편집기가 아니라 happy-dom을
시험하는 꼴이 된다. 그래서 실제 검증은 **headless Chrome(google-chrome
`--headless=new --dump-dom`)** 에서 돌렸고, 마지막에는 빌드한
`MjmlBlockEditor.vue` 자체를 브라우저에 올려 확인했다.

## 결과

| 항목                                         | 결과                                                                                                                                                                                        |
| -------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| (a) Vue 3 + Vite 7에서 동적 import           | **통과.** SSR 문제 없음 — 모듈 최상위에서 `document`를 만지지 않고, 컴포넌트가 `onMounted`에서만 `grapesjs.init`을 호출한다.                                                                |
| (b) 서버 컴파일러가 받는 MJML                | **통과.** `<mjml><mj-body>…` 문서를 그대로 내보낸다. import → export → 재import가 문자열까지 동일(idempotent).                                                                              |
| (c) 커스텀 블록 3종                          | **통과.** i18n 텍스트 / 조건부 섹션 / 수신거부 링크 모두 팔레트에 뜨고, export·프로젝트 재적재·strict 검증을 통과한다.                                                                      |
| (d) `getProjectData()` / `loadProjectData()` | **통과.** 본문 하나짜리 문서에서 3.4 KB, 커스텀 블록 3개를 얹어 5.8 KB. 재적재 후 export 결과가 바이트 단위로 같다.                                                                         |
| (e) 지연 청크 크기                           | `@sendplane/ui` dist `MjmlBlockEditor-*.js` **69.10 kB (gzip 15.69 kB)** — 대부분이 인라인한 GrapesJS CSS. 콘솔 앱 번들 기준 **2,476 kB (gzip 691 kB)**. 블록 탭을 열기 전에는 받지 않는다. |
| (f) `mj-head` 없는 문서                      | **통과.** `mj-head`를 전혀 만들지 않으며 strict 컴파일도 정상. `{{ content }}` 슬롯은 Layout 쪽에 그대로 두면 된다.                                                                         |

## 반드시 지켜야 할 것 (gotcha)

### 1. Liquid 연산자가 HTML 이스케이프된다 — 후처리 필수

GrapesJS는 문서를 DOM으로 파싱했다가 다시 직렬화하므로 텍스트·속성값 안의
`<`, `>`, `&`가 엔티티로 나온다. `mj-text`든 `mj-raw`든 똑같다.

```
입력   <mj-text>{% if a > b %}…</mj-text>
export <mj-text>{% if a &gt; b %}…</mj-text>   ← Liquid가 파싱 못 함
```

MJML은 이걸 그대로 통과시키므로 엔티티가 HTML 템플릿까지 살아남아 발송 시
Liquid 파싱이 깨진다. 그래서 `lib/mjml-blocks.ts`의 `unescapeLiquid()`가
**`{{ … }}` / `{% … %}` 구간 안에서만** 엔티티를 되돌린다. 구간 밖(`AT&amp;T`
같은 본문)은 건드리지 않고, 한 겹만 푼다(`&amp;gt;` → `&gt;`).

반대로 `{% t "key" %}`의 큰따옴표와 `{{ recipient.name }}`은 원래
이스케이프되지 않으므로 따로 손댈 것이 없다.

### 2. `mj-*` 태그에 `data-*` 속성을 붙이면 publish가 깨진다

`internal/render.compileMJML`은 `mjml.WithValidationLevel(mjml.Strict)`로
컴파일한다. MJML strict 검증은 모르는 속성을 **오류로 취급**한다:

```
ValidationError: Line 1 of . (mj-button) — Attribute data-sp-track is illegal
ValidationError: Line 1 of . (mj-text)   — Attribute data-sp-i18n is illegal
```

그래서 블록 정의는 마커를 전부 **`css-class`** 에 싣는다(MJML 정식 속성이고
커스텀 타입 재인식에도 충분하다). 수신거부 링크는 `mj-button`이 아니라
`mj-text` 안의 평범한 `<a href="{{ unsubscribe_url }}" data-sp-track="off">`로
만든다 — `mj-text`의 자식 HTML은 MJML이 검증 없이 통과시키므로 속성이
컴파일 결과 HTML까지 그대로 살아남는다(실측 확인).

### 3. 기타

- 빈 문서에서 `mjml-code`는 빈 문자열을 돌려준다 → 본문이 없으면
  `DEFAULT_MJML` 골격을 심어 두어야 한다.
- `editor.getWrapper().find(...)`는 캔버스 iframe 준비 전에는 빈 배열을
  돌려준다. 초기화 이후 작업은 전부 `editor.onReady()` 안에서 한다.
- `grapesjs/dist/css/grapes.min.css`(61 kB)를 평범한 CSS import로 넣으면
  라이브러리 빌드가 `sendplane-ui.css` 한 장으로 합쳐 **모든 호스트가 항상**
  내려받게 된다. `?inline`으로 문자열로 받아 마운트 시 `<style>`로 주입하고
  참조 계수로 정리한다.
- `mj-image` 같은 self-closing 태그를 `<mj-image />`로 쓰면 HTML 파서가 뒤
  형제를 자식으로 빨아들인다. 저장되는 MJML은 편집기가 다시 써 주므로
  문제되지 않지만, 손으로 쓴 MJML을 blocks 탭으로 가져올 때는 닫는 태그를
  쓰는 편이 안전하다(플러그인의 `useXmlParser` 옵션도 있으나 실험 단계).

## 서버(Go) 쪽에 필요한 것

지금 코드 기준으로 **필수 변경은 없다.** blocks 모드는 이미
`effectiveMode`가 MJML로 취급하고, `Template.Blocks`는 그대로 보관된다.
다만 확인·검토가 필요한 지점:

- `data-sp-track="off"`는 publish의 `markUnsubscribeAnchors`가 어차피 붙여
  주는 값이다. 이미 붙어 있으면 건너뛰도록 되어 있어(`postprocess.go`) 중복
  속성은 생기지 않는다 — 편집기가 먼저 넣어 두면 `{{ content }}` 슬롯 병합
  전후로 판정이 흔들릴 여지가 줄어든다.
- strict 검증을 유지하는 한 위 (2)의 제약은 계속 유효하다. 완화할 생각이라면
  ADR-0009에 근거를 남기는 편이 좋다.
