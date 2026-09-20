# sendplane 프론트엔드

sendplane 운영 콘솔과 그 구성 요소를 담는 pnpm 워크스페이스입니다.
설계 근거는 [`docs/architecture.md` §13](../docs/architecture.md)과
[ADR-0010](../docs/adr/0010-frontend-packages.md)을 참조하세요.

핵심 원칙은 **"전체 앱을 통째로 가져다 쓰는 것"과 "페이지 하나만 가져다 쓰는 것"이 모두
1급 사용법**이라는 점입니다. 그래서 라우팅·인증·테마는 호스트 앱이 소유하고,
`@sendplane/ui`는 라우터에 의존하지 않습니다.

---

## 패키지 구성

| 경로           | 이름                 | 역할                                                                            |
| -------------- | -------------------- | ------------------------------------------------------------------------------- |
| `packages/api` | `@sendplane/api`     | `api/openapi.yaml`에서 생성한 타입 + 얇은 fetch 클라이언트. **프레임워크 무관** |
| `packages/ui`  | `@sendplane/ui`      | Vue 3 프리미티브 + 페이지 컴포넌트. **vue-router 의존 없음**                    |
| `apps/console` | `@sendplane/console` | Vite SPA. 라우터 · 인증 어댑터 · 로케일/테마 스위처 조립                        |

```
web/
├── pnpm-workspace.yaml
├── tsconfig.base.json        # 공통 컴파일러 옵션
├── eslint.config.js          # flat config (vue + ts + prettier)
├── packages/
│   ├── api/
│   │   ├── src/schema.d.ts   # 생성물(커밋 대상). `pnpm gen`으로 재생성
│   │   ├── src/client.ts     # createClient(), 스트리밍 인제스트, YAML 래퍼
│   │   ├── src/errors.ts     # SendplaneError (스펙의 Error 스키마 매핑)
│   │   └── src/ndjson.ts     # NDJSON 인코딩, CSV/NDJSON 파일 리더
│   └── ui/
│       ├── src/routes.ts     # 라우트 매니페스트 {name, path, page}
│       ├── src/context.ts    # provide/inject 되는 {client, navigate, href, t, locale}
│       ├── src/theme.css     # --sp-* 디자인 토큰
│       ├── src/i18n/         # en.json / ko.json (호스트가 병합 가능)
│       ├── src/components/   # Button, Table, StatusBadge, Card, Tabs, …
│       └── src/pages/        # CampaignDetailPage, TemplateEditorPage, …
└── apps/console/
    └── dist/                 # `pnpm build` 산출물. Go 바이너리가 embed.FS로 서빙
```

### 요구 환경

Node 22, pnpm 10.

```bash
cd web
pnpm install
```

리포지터리 루트에서는 `make web-install` / `make web-gen` / `make web-lint` /
`make web-test` / `make web-build` 로도 같은 일을 할 수 있습니다.

---

## 스크립트

| 명령             | 하는 일                                                    |
| ---------------- | ---------------------------------------------------------- |
| `pnpm gen`       | `api/openapi.yaml` → `packages/api/src/schema.d.ts` 재생성 |
| `pnpm lint`      | ESLint (vue + ts)                                          |
| `pnpm typecheck` | `tsc` / `vue-tsc` 전체                                     |
| `pnpm test`      | vitest (api 25 · ui 49 · console 6)                        |
| `pnpm build`     | api(tsc) → ui(vite lib) → console(vite) 순서로 빌드        |
| `pnpm format`    | Prettier                                                   |

`pnpm -r`는 워크스페이스 의존 관계를 따라 **위상 순서로** 실행하므로
`pnpm build` 한 번이면 `@sendplane/api` → `@sendplane/ui` → `@sendplane/console`
순서가 보장됩니다.

---

## 클라이언트 재생성

DTO는 손으로 쓰지 않습니다. 스펙이 단일 진실 공급원입니다.

```bash
cd web
pnpm gen                                    # packages/api/src/schema.d.ts 갱신
git diff packages/api/src/schema.d.ts       # 변경 확인 후 커밋
```

CI의 `web` 잡은 `pnpm gen` 뒤에 `git diff --exit-code`를 돌려서
**스펙을 고치고 생성물을 커밋하지 않은 경우 빌드를 깹니다.** 생성물이 바뀌면
`@sendplane/ui`에서 타입 에러로 드리프트가 즉시 드러납니다(ADR-0010).

---

## `@sendplane/api` 사용법

```ts
import { createClient, SendplaneError, isSendplaneError } from '@sendplane/api'

const client = createClient({
  baseUrl: '', // 비우면 same-origin
  getAuthHeaders: () => ({ 'X-API-Key': myKey }), // 동기/비동기 모두 가능
})
```

두 가지 호출 스타일을 제공합니다.

```ts
// 1) openapi-fetch 원형: { data, error, response }
const { data, error } = await client.GET('/api/v1/campaigns', {
  params: { query: { limit: 50 } },
})

// 2) 언랩 형태: 성공하면 페이로드, 실패하면 SendplaneError를 throw
const page = await client.get('/api/v1/campaigns', { params: { query: { limit: 50 } } })
```

`SendplaneError`는 스펙의 `Error` 스키마(`code` / `message` / `details`)를 그대로
담고, 응답을 받지 못한 경우 `code: 'network_error'`, `status: 0`을 씁니다.
분기는 **항상 `code`로** 하세요(스펙이 명시).

```ts
try {
  await client.post('/api/v1/templates/{templateId}/publish', { params: { path: { templateId } } })
} catch (e) {
  if (isSendplaneError(e) && e.code === 'missing_i18n_keys') showTranslationPanel()
  else if (isSendplaneError(e) && e.isVersionConflict) reloadAndReapply()
}
```

### 수신자 스트리밍 인제스트

`POST /campaigns/{id}/recipients`는 JSON이 아니라 NDJSON 스트림이므로 전용 래퍼가 있습니다.

```ts
import { recipientsFromFile } from '@sendplane/api'

const result = await client.ingestRecipients(campaignId, recipientsFromFile(file), {
  idempotencyKey: `${file.name}-${file.size}-chunk-0`,
  onProgress: ({ lines, bytes }) => setProgress(lines),
})
// → { accepted, duplicates, invalid, total, idempotent_replay? }
```

- 런타임이 요청 본문 스트리밍을 지원하면 `ReadableStream` + `duplex: 'half'`로 보냅니다.
  지원하지 않으면(현재 Safari·Firefox) 자동으로 **버퍼링 경로로 폴백**합니다.
  판정은 `supportsRequestStreams()`로 직접 확인할 수도 있습니다.
- 입력은 `RecipientLine` 객체의 (async) iterable, `ReadableStream`, 이미 NDJSON인
  바이트/문자열 청크 중 무엇이든 됩니다.
- `recipientsFromFile(file)`은 확장자로 CSV/NDJSON을 골라 **스트리밍으로** 읽으므로
  100만 행 파일도 브라우저 메모리에 쌓이지 않습니다. CSV는 `email` 컬럼이 필수이고,
  `vars.plan` 같은 컬럼은 `vars`에 중첩 저장됩니다.
- 청크 단위로 나눠 호출하고 청크마다 다른 `Idempotency-Key`를 주면, 중간에 끊긴 업로드를
  다시 돌려도 완료된 청크는 저장된 결과만 돌려받습니다(중복 삽입 없음).

### 딜리버리 목록

```ts
const page = await client.listDeliveries({
  email: 'a+b@example.com', // 주소로 캠페인을 가로질러 검색
  lane: ['transactional'], // 캠페인 없는 딜리버리만
  status: ['failed', 'bounced'],
  since: '2025-03-01T00:00:00Z',
  limit: 50,
})
```

`GET /api/v1/deliveries`를 감싼 얇은 편의 메서드입니다(쿼리 타입은
`DeliveryQuery`). 캠페인 스코프가 필요하면 `campaign_id`를 주거나
`client.get('/api/v1/campaigns/{campaignId}/deliveries', …)`를 그대로 쓰면 됩니다.

### i18n YAML

```ts
const yaml = await client.getI18nYaml(templateId) // GET …/i18n?format=yaml
const summary = await client.putI18nYaml(templateId, yaml) // PUT …/i18n (x-yaml)
```

---

## `@sendplane/ui`를 페이지 단위로 쓰기

`@sendplane/ui`는 **라우터를 모릅니다.** 대신

- `client` — 데이터를 가져올 클라이언트
- `navigate(to)` — `{ name, params }`로 오는 이동 요청을 호스트 라우터로 넘기는 콜백
- `href(to)` — 같은 대상의 URL (링크가 진짜 `<a>`로 남도록)
- `t`, `locale` — vue-i18n 메시지(en/ko 동봉, 호스트가 병합·교체 가능)

를 `provide/inject`로 받습니다. 라우트 이름은 `routes.ts` 매니페스트가 정의합니다.

### 최소 예제: 호스트 앱이 캠페인 상세 한 장만 쓰기

```ts
// router.ts — 호스트가 자기 라우팅 테이블을 소유합니다.
import { createRouter, createWebHistory } from 'vue-router'
import MailCampaign from './MailCampaign.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    // 호스트가 원하는 경로에 마운트하고, 이름만 매니페스트와 맞춰 둡니다.
    { name: 'campaign', path: '/admin/mail/:campaignId', component: MailCampaign, props: true },
  ],
})
```

```vue
<!-- MailCampaign.vue -->
<script setup lang="ts">
import { createClient } from '@sendplane/api'
import { CampaignDetailPage, SendplaneProvider, type NavigateTarget } from '@sendplane/ui'
import '@sendplane/ui/style.css'
import { useRouter } from 'vue-router'

defineProps<{ campaignId: string }>()

const router = useRouter()
const client = createClient({
  baseUrl: '/mail', // 호스트가 핸들러를 마운트한 위치
  getAuthHeaders: async () => ({ Authorization: `Bearer ${await getToken()}` }),
})

const navigate = (to: NavigateTarget) => router.push({ name: to.name, params: to.params })
const href = (to: NavigateTarget) => router.resolve({ name: to.name, params: to.params }).href
</script>

<template>
  <SendplaneProvider
    :client="client"
    :navigate="navigate"
    :href="href"
    locale="ko"
    :messages="{ ko: { campaign: { title: '메일 발송' } } }"
  >
    <CampaignDetailPage :campaign-id="campaignId" />
  </SendplaneProvider>
</template>
```

포인트:

- 페이지는 **props로 id만 받고 자기 데이터는 스스로 가져옵니다.** 부모가 미리 로드해 줄 필요가 없습니다.
- `messages`는 동봉된 번들 위에 깊은 병합됩니다. 번역기를 통째로 바꾸려면 `:t="myT"`를 주세요.
- `SendplaneProvider`는 토스트/확인 다이얼로그 호스트도 함께 렌더합니다.
  호스트가 자기 것을 쓰려면 `:overlays="false"`로 끄면 됩니다.
- `SendplaneProvider` 대신 호스트 컴포넌트의 `setup()`에서 `provideSendplane({...})`을
  직접 호출해도 됩니다.

### 라우트 매니페스트로 전체를 마운트하기

```ts
import { routes as manifest } from '@sendplane/ui'

const records = manifest.map((route) => ({
  name: route.name, // 'campaign', 'template', 'senders', …
  path: `/admin/mail${route.path}`,
  component: route.page, // () => import(...) — lazy loader
  props: true, // 경로 파라미터 이름 = 페이지 prop 이름
}))
```

사이드바에 쓸 항목은 `navRoutes`가 걸러 줍니다(각 항목의 `navKey`가 메시지 키).

### 테마

토큰은 전부 `--sp-*` CSS 변수입니다. 다크 모드는 기본적으로
`prefers-color-scheme`을 따르고, `<html data-theme="light|dark">`로 고정할 수 있습니다.
호스트는 아무 조상 엘리먼트에서 변수만 재정의하면 됩니다.

```css
:root {
  --sp-accent: #c2410c;
  --sp-radius: 2px;
}
```

### 무거운 에디터

MJML/HTML 탭의 코드 에디터는 `@codemirror/*`를 **동적 import**하므로 별도 청크로
분리되고, 에디터 화면을 열지 않으면 로드되지 않습니다. 블록 에디터
`MjmlBlockEditor.vue`(GrapesJS + grapesjs-mjml, ADR-0009)도 마찬가지로 동적
import라 **블록 탭을 열 때만** 내려받습니다(콘솔 빌드 기준 약 2.4 MB / gzip
691 kB). 저장 형식은 `Template.blocks`에
`{editor, editor_version, project, mjml}`이고 서버는 `Template.body`의 MJML만
컴파일합니다. 스파이크 결과와 주의사항은
[`packages/ui/docs/block-editor-spike.md`](packages/ui/docs/block-editor-spike.md)에
정리해 두었습니다. 호스트가 자체 블록 에디터를 가지고 있다면
`TemplateEditorPage`의 `#block-editor` 슬롯으로 끼워 넣으면 됩니다.

---

## `@sendplane/console`

참조 운영 콘솔입니다.

```bash
cd web
pnpm --filter @sendplane/console dev      # http://localhost:5173
```

- **API 주소**: `VITE_SENDPLANE_API`가 있으면 그 오리진, 없으면 same-origin.
  개발 서버는 `VITE_SENDPLANE_API`가 없을 때 `/api`와 `/t`를
  `http://localhost:8080`으로 프록시합니다.
- **인증**: API 키를 한 번 입력받아 `sessionStorage`에 보관합니다(탭을 닫으면 로그아웃).
  `X-API-Key` 헤더로 실려 나가고, 401을 받으면 키를 지워 입력 화면으로 되돌립니다.
  테넌트는 절대 클라이언트가 보내지 않습니다 — 호스트의 `TenantResolver`가
  principal에서 유도합니다.
- **로케일/테마**: en·ko 스위처와 system/light/dark 토글이 `localStorage`에 저장됩니다.

빌드 산출물:

```
web/apps/console/dist/
```

Go 바이너리가 이 디렉터리를 `embed.FS`로 서빙합니다(ADR-0010).
루트가 아닌 경로에 마운트한다면 `VITE_BASE=/admin/mail/ pnpm build`처럼 base를 주세요.

---

## 테스트

```bash
cd web
pnpm test
```

- `packages/api` — fetch 목으로 인증 헤더 주입, `explode=true` 쿼리 직렬화,
  `SendplaneError` 매핑, NDJSON 스트리밍/버퍼링 양쪽 경로, CSV/NDJSON 리더,
  `listDeliveries`의 쿼리 직렬화를 검증합니다.
- `packages/ui` — provider/composable, `StatusBadge`, `Table`의 커서 페이지네이션,
  목 클라이언트를 물린 `CampaignDetailPage`, `TemplateEditorPage`의 i18n 누락 키 판정과
  모드 전환 확인을 검증합니다. 블록 에디터는 순수 헬퍼(`lib/mjml-blocks.ts`)만
  검증하고 컴포넌트 자체는 **skip**입니다 — happy-dom의 `Attr.nodeName`이 비어 있어
  GrapesJS 파서가 속성을 전부 잃기 때문입니다(자세한 내용은 스파이크 문서).
- `apps/console` — 매니페스트로 만든 라우터가 모든 라우트 이름을 등록하는지, API 키 게이트와
  로케일 스위처가 동작하는지 스모크 테스트합니다.

---

## 알려진 스펙상의 거친 부분

프론트를 붙이면서 걸렸던 것들입니다. 1~6번은 스펙 쪽에서 정리됐고, 아래는
**무엇이 어떻게 바뀌었는지**와 **아직 남은 것**입니다.

### 정리된 것

1. **테넌트 전역 딜리버리 목록이 생겼습니다.** `GET /api/v1/deliveries`가
   `campaign_id` · `lane` · `status` · `error_class` · `email` · `since` ·
   `until` · `limit` · `cursor`를 받습니다(액션은 `delivery.read`). 그래서
   "주소로 전체 검색"과 "트랜잭셔널 딜리버리 조회"(`lane=transactional`)가
   이제 가능합니다. `DeliveryListPage`는 `campaignId`를 **선택 prop**으로 낮추고
   이 엔드포인트로 갈아타면 됩니다. 클라이언트에는
   `client.listDeliveries(query)` 편의 메서드와 `LANES` 상수가 있습니다.
   캠페인 스코프 `GET /campaigns/{id}/deliveries`는 그대로입니다 — 같은 목록에
   캠페인 존재 확인(404)만 얹은 형태입니다.
2. **요청 바디 속성의 `default:`를 전부 걷어냈습니다.** 기본값은 `description`에만
   적고 서버가 적용합니다. `ProbeMailboxInput.inbox_folder` / `enabled`,
   `BounceMailboxInput.folder` / `after_process` / `enabled`,
   `PublishRequest.allow_missing_i18n_keys`, `MessageRequest.priority`,
   `UnsubscribeNotice.source`가 모두 **optional**로 생성됩니다.
3. **`TenantSettings`에 `unsubscribe_one_click`과 `bounce_retain_raw`가 생겼습니다.**
   둘 다 이미 엔진(`internal/sender`, `internal/bounce`)이 읽던 값인데 API에만
   없었습니다. 설정 화면에서 진짜 토글로 만들 수 있습니다.
4. **`CampaignStats`에 `total`과 `sent`가 명시됐습니다.** `sent`는
   `by_status.sent + bounced + complained`입니다 — 며칠 뒤 도착한 바운스가
   이미 계산된 오픈율의 분모를 줄이면 안 되기 때문입니다. `by_status`는 그대로
   남아 있습니다.
5. **i18n YAML 표현의 스키마가 `type: string`이 됐습니다.** 생성 타입만 봐도
   YAML 응답이 문자열이라는 게 드러납니다. `getI18nYaml` / `putI18nYaml` 래퍼는
   그대로 둡니다 — 타입이 정직해졌을 뿐, `openapi-fetch`는 한 오퍼레이션에
   미디어 타입 하나만 태우기 때문입니다.
6. **`/suppressions/{email}`은 경로 형태 그대로 둡니다.** 대신 스펙이 인코딩을
   못박았습니다: 세그먼트를 `encodeURIComponent`로 감싸서
   `a%2Bb%40example.com`으로 보내세요. `+`가 들어간 주소가 `%2B`로도 그냥 `+`로도
   같은 엔트리에 닿는다는 것은 Go 쪽 라우터 테스트
   (`TestSuppressionPathTakesAnEncodedAddress`)가 고정합니다.

### 아직 남은 것

7. **낙관적 동시성의 `version`이 응답에서 `readOnly`**라 업데이트 바디에 다시 넣어야
   하는데, 생성 타입상 `TransportUpdate` 등은 `version`을 요구하므로 화면이 항상
   "읽은 객체"를 들고 있어야 합니다. 현재 편집 화면들이 그렇게 되어 있습니다.
