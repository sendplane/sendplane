# internal/render

템플릿 → 메일 3종(subject / html / text)을 만드는 패키지입니다.
설계 근거는 [architecture.md §6, §9.2, §16](../../docs/architecture.md), [ADR-0004](../../docs/adr/0004-liquid-i18n-escaping.md), [ADR-0009](../../docs/adr/0009-mjml-publish-time.md).

## 파이프라인

```
[publish · control 프로세스, 1회]
  Template(.Body, .I18n) + Layout(.Body, .I18n)
    → mergeLayout      : Layout 본문의 {{ content }} 슬롯에 Template 본문 삽입
    → mergeBundles     : Layout 번들 + Template 번들 (Template 키 우선, default_locale은 Template 것)
    → compileMJML      : blocks | mjml 모드만 mjml-go(WASM)로 1회 컴파일. html 모드는 통과
    → markUnsubscribe  : {{ unsubscribe_url }} 앵커에 data-sp-track="off" 부착
    → HTMLToText       : Template.Text 가 비었을 때 HTML에서 자동 생성 (Liquid 보존)
    → Validate/ExtractKeys : 3개 파트가 Liquid로 파싱되는지, 쓰인 t 키가 기본 로케일에 있는지
    → ExtractLinks     : 추적 대상 http(s) <a href> 를 문서 순서로 수집 → link_no
    → Checksum(sha256) : subject/html/text/default_locale/번들
  = store.MessageVersion (불변)

[send · sender 프로세스, 수신자마다]
  Renderer.PrepareChain(version, recipient.locale, campaign.default_locale)
    → *Prepared (3개 파트 파싱 1회, LRU 캐시)
  Prepared.Render(ctx, Bindings)
    → 바인딩 조립(vars ← campaign vars에 recipient vars 덮어쓰기)
    → HTML 파트는 이스케이프된 바인딩, subject/text는 원본 바인딩
    → Liquid 렌더 (렌더 타임아웃 + 출력 크기 상한)
    → subject의 CR/LF 거부 (ErrHeaderInjection)
  = Output{Subject, HTML, Text} + []Warning

[post-process · 9.2, 토큰 서명은 호출자 몫]
  RewriteLinks(html, func(linkNo, href) string)   // linkNo == MessageVersion.Links 인덱스
  InsertPixel(html, pixelURL)                      // </body> 앞, 없으면 끝에 append
```

## 캐시 키

| 캐시 | 키 | 크기 | 위치 |
|---|---|---|---|
| 파싱된 메시지 파트 | `version.ID \0 strings.Join(locales, ",")` | `WithCacheSize`, 기본 256 | `Renderer` |
| 파싱된 번역값 | `(번들 sha256, 해석된 로케일, 키)` | `WithTranslationCacheSize`, 기본 4096 | `Engine` |

둘 다 LRU이고 mutex로 보호됩니다. `*Prepared` 하나를 32개 고루틴이 동시에 Render해도 안전합니다
(osteele/liquid의 파싱 트리는 렌더 중 불변이고, 바인딩 맵은 렌더마다 복사됩니다 — `TestConcurrentRender`).

## 로케일 폴백 (§6.2)

`PrepareChain(v, recipient.Locale, campaign.DefaultLocale)` →
`recipient.locale` → 언어만(`ko-KR`→`ko`) → `campaign.default_locale` → 그 언어 → `version.DefaultLocale` → 그 언어 → 번들 default → **키 문자열 + `missing_key` 경고**.
경고는 로그가 아니라 `[]Warning`으로 반환됩니다(API 미리보기 응답 / sender 이벤트용).

`Prepare(v, locale)`는 `PrepareChain(v, locale)`의 별칭입니다. 캠페인 기본 로케일이 있으면 `PrepareChain`을 쓰세요.

## 이스케이프 규칙 (ADR-0004)

- **HTML 파트**: 바인딩의 모든 문자열을 렌더 **전에** 재귀적으로 `html.EscapeString`. map/slice는 새로 만들어 반환하므로 호출자 데이터는 변형되지 않습니다.
- **subject / text 파트**: 이스케이프 없음.
- **신뢰 HTML**: `{"$html": "<b>..</b>"}` 형태의 **키가 하나뿐인** map만 원본으로 통과합니다. 다른 키가 섞여 있으면 일반 map으로 보고 이스케이프합니다.
- **번역값**: 번역 문자열 자체는 작성자가 쓴 것이므로 마크업이 허용되고, 그 안에서 참조하는 변수는 이미 이스케이프된 바인딩입니다.
- Liquid 샌드박스: `include` / `render` 태그 제거, 파일 접근을 거부하는 TemplateStore, 렌더 타임아웃(ctx), 파트별 출력 상한(기본 2 MiB), 렌더 중 panic을 error로 변환.

## 라이브러리 선택

| 용도 | 선택 | 이유 |
|---|---|---|
| Liquid | `github.com/osteele/liquid` v1.9.2 | ADR-0004의 결정. `UnregisterTag`로 `include`/`render`를 제거할 수 있고, 파싱 결과를 동시 렌더에 재사용할 수 있음 |
| MJML | `github.com/Boostport/mjml-go` v0.16.0 | ADR-0009. Node 불필요(wazero WASM). 내부적으로 1~10개 인스턴스 풀을 유지해 **자체 스레드 안전**이라 별도 세마포어를 두지 않았습니다 |
| YAML | `gopkg.in/yaml.v3` v3.0.1 | `yaml.Node`로 직접 조립해 로케일/키 정렬을 보장 → export 결과가 결정적이고 diff 가능 |
| HTML 파싱 | `golang.org/x/net/html` (기존 의존성) | 토크나이저만 사용 |
| HTML→text | **자체 구현** (`html2text.go`, x/net/html 토크나이저 기반) | 아래 참조 |

### HTML→text를 직접 구현한 이유 (문서와의 차이)

`jaytaylor/html2text`와 `k3a/html2text`를 실제 MJML 산출물로 비교한 결과 둘 다 이 파이프라인에는 맞지 않았습니다.

- `jaytaylor/html2text`: 출력 품질은 좋지만 **DOM 트리를 만들기 때문에** `<mj-raw>`가 테이블 행 사이에 넣은 `{% if %}` / `{% endif %}` 텍스트 노드가 HTML5의 foster parenting 규칙으로 테이블 **앞으로 끌려나옵니다**. text 파트는 publish 시점에 만들어지는 Liquid 템플릿이므로, 이는 조건문이 지키던 내용과 분리되는 **조용한 버그**입니다. 게다가 `go.mod`가 없고 `olekukonko/tablewriter`를 끌고 옵니다.
- `k3a/html2text`: 의존성이 없고 작지만 정규식 기반이라 MJML 산출물 전체가 **한 줄로 뭉개지고** 링크 URL이 사라집니다.

자체 구현은 트리를 만들지 않는 토크나이저로 훑기 때문에 문서 순서가 보존되고, 블록 요소에서 줄을 바꾸고, `<a>`는 `텍스트 ( URL )` 형태로 남기며, `{% if %}`처럼 **출력이 없는 제어 태그만 있는 줄**은 앞뒤 빈 줄을 만들지 않습니다(조건이 거짓일 때 빈 줄이 쌓이지 않도록). `<head>`/`<style>`/MSO 조건부 주석은 버립니다.

### `{{ "key" | t }}` 필터 형식 처리 (구현상의 차이)

osteele/liquid의 필터는 **렌더 컨텍스트에 접근할 수 없는 평범한 함수**라서, 필터 구현에서는 현재 로케일도 바인딩도 볼 수 없고 번역값 안의 Liquid를 렌더할 수도 없습니다. 커스텀 **태그**는 컨텍스트를 받습니다.
그래서 파싱 직전에 `{{ expr | t }}` → `{% t expr %}`, `{{ expr | t: name: x }}` → `{% t expr, name: x %}`로 **소스 수준에서 정규화**합니다(`rewriteTFilter`). trim 마커(`{{-`/`-}}`)는 보존됩니다.
제약 두 가지:
- `t`는 필터 체인의 **마지막**이어야 하며, 아니면 `ErrTFilterChain`입니다(`{{ "k" | t | upcase }}` 불가).
- 이 정규화는 소스 전체를 훑으므로 `{% raw %}` 블록 안의 `{{ "k" | t }}`도 태그 형식으로 바뀝니다. 즉 `{% raw %}` 안에서 필터 형식을 그대로 보여 주는 용도로는 쓸 수 없습니다.

## link_no 정렬 규칙

publish의 `ExtractLinks`와 send의 `RewriteLinks`는 같은 판정 함수(`isTrackable`)를 씁니다: 빈 href / `#앵커` / `mailto:` / `tel:` / http(s)가 아닌 스킴 / `data-sp-track="off"`는 제외.
`{{ unsubscribe_url }}` 앵커는 렌더 후에는 평범한 https URL이 되어 버리므로, **publish 시점에 `data-sp-track="off"`를 붙여** 양쪽에서 동일하게 제외되도록 만듭니다.

남은 주의점: href 전체가 Liquid 변수라서 publish 시 스킴을 알 수 없는 링크는 http(s)로 **가정**합니다. 그것이 발송 시 `mailto:` 같은 값으로 렌더되면 그 뒤 링크들의 `link_no`가 한 칸씩 밀립니다. 동적 href는 http(s)로 유지하세요.

## 공개 API

```go
Publish(ctx, tpl, layout, PublishOptions) (*store.MessageVersion, []Warning, error)

NewRenderer(...Option) *Renderer
(*Renderer).Prepare(v, locale) / PrepareChain(v, locales...) (*Prepared, error)
(*Prepared).Render(ctx, Bindings) (Output, []Warning, error)

NewEngine(...EngineOption) *Engine      // Parse / Validate

RewriteLinks(html, func(linkNo int, href string) string) (string, error)
InsertPixel(html, pixelURL string) string
ExtractLinks(html string) ([]string, error)
HTMLToText(html string) string

ExportYAML(store.I18nBundle) ([]byte, error)
ImportYAML([]byte) (store.I18nBundle, []Warning, error)
ExtractKeys(subject, body, text string) ([]string, error)
```

## 테스트

```
go test -race ./internal/render/...
go test ./internal/render/ -update        # testdata/golden/* 갱신
go test -run XXX -bench . ./internal/render/
```

`testdata/`에 MJML 레이아웃·템플릿과 ko/en 번들 픽스처가 있고, `testdata/golden/`에 publish 산출물과 로케일별 렌더 결과를 커밋해 두었습니다.
MJML 컴파일 벤치마크(`BenchmarkMJMLCompile`)의 목표는 컴파일당 500 ms 미만입니다.
