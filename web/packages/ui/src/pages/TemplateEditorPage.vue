<script setup lang="ts">
import type {
  ContentMode,
  I18nBundle,
  I18nKeyList,
  I18nKeyUsage,
  PreviewRecipient,
  Template,
  TenantVars,
  Vars,
} from '@sendplane/api'
import { computed, defineAsyncComponent, onMounted, onUnmounted, ref, watch } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import SpTabs, { type TabItem } from '../components/SpTabs.vue'
import SpTextarea from '../components/SpTextarea.vue'
import TenantVarsEditor from '../components/TenantVarsEditor.vue'
import { useApiToast } from '../composables/useApiToast.js'
import { useAsync } from '../composables/useAsync.js'
import { useConfirm } from '../composables/useConfirm.js'
import { useSendplane } from '../context.js'
import { downloadText, tryParseJson } from '../lib/format.js'
import { buildI18nMatrix, i18nSummaryKind } from '../lib/i18n-matrix.js'
import { extractI18nKeys } from '../lib/mjml-blocks.js'

const props = defineProps<{ templateId?: string }>()

const { client, t, navigate, systemTenant, tenantId } = useSendplane()
const toast = useApiToast()
const confirm = useConfirm()

// Both editors are heavy and mutually exclusive, so each is its own chunk.
const SpCodeEditor = defineAsyncComponent(() => import('../components/SpCodeEditor.vue'))
const SpBlockEditorSlot = defineAsyncComponent(() => import('../components/SpBlockEditorSlot.vue'))

const draft = ref({
  name: '',
  subject: '',
  preheader: '',
  mode: 'mjml' as ContentMode,
  body: '',
  text: '',
  layout_id: '',
  default_locale: 'en',
})
const blocks = ref<Record<string, unknown> | undefined>()
const current = ref<Template | undefined>()
const saving = ref(false)
const publishing = ref(false)
const allowMissingKeys = ref(false)

const loaded = useAsync(
  (signal) =>
    props.templateId
      ? client.get('/api/v1/templates/{templateId}', {
          params: { path: { templateId: props.templateId } },
          signal,
        })
      : Promise.resolve(undefined as Template | undefined),
  { watch: () => props.templateId },
)

watch(loaded.data, (template) => {
  if (!template) return
  current.value = template
  draft.value = {
    name: template.name,
    subject: template.subject,
    preheader: template.preheader ?? '',
    mode: template.mode,
    body: template.body ?? '',
    text: template.text ?? '',
    layout_id: template.layout_id ?? '',
    default_locale: template.default_locale ?? 'en',
  }
  blocks.value = template.blocks
})

const layouts = useAsync((signal) =>
  client.get('/api/v1/layouts', { params: { query: { limit: 200 } }, signal }),
)

/** Keys the server extracted by parsing the subject, body, text and layout. */
const i18nKeys = useAsync(
  (signal) =>
    props.templateId
      ? client.get('/api/v1/templates/{templateId}/i18n/keys', {
          params: { path: { templateId: props.templateId } },
          signal,
        })
      : Promise.resolve(undefined),
  { watch: () => props.templateId },
)

const i18nBundle = useAsync(
  (signal) =>
    props.templateId
      ? client
          .get('/api/v1/templates/{templateId}/i18n', {
            params: { path: { templateId: props.templateId }, query: { format: 'json' as const } },
            signal,
          })
          // The endpoint's 200 has two content types (`application/json` and
          // `application/x-yaml`, the latter used by `getI18nYaml`), so the
          // generated type is a union; `format: 'json'` guarantees the JSON
          // shape at runtime.
          .then((data) => data as I18nBundle)
      : Promise.resolve(undefined),
  { watch: () => props.templateId },
)

const modeTabs = computed<TabItem[]>(() =>
  (['blocks', 'mjml', 'html'] as const).map((mode) => ({
    value: mode,
    label: t(`template.modes.${mode}`),
  })),
)

/**
 * Mode switches are confirmed because they move authority over the body.
 *
 * Leaving `blocks` keeps the exported MJML in `draft.body` — the code editor
 * simply takes over editing it — but further code edits no longer reach the
 * saved GrapesJS project, so the two drift apart. Entering `blocks` re-imports
 * the MJML, and anything GrapesJS cannot model is dropped on the way in.
 */
async function changeMode(next: string) {
  const mode = next as ContentMode
  if (mode === draft.value.mode) return
  const leavingBlocks = draft.value.mode === 'blocks'
  const enteringBlocks = mode === 'blocks'
  if (draft.value.body.trim() && (leavingBlocks || enteringBlocks)) {
    const message = leavingBlocks ? t('blockEditor.switchToCode') : t('blockEditor.switchToBlocks')
    if (!(await confirm(message))) return
  }
  draft.value.mode = mode
}

const layoutOptions = computed(() =>
  (layouts.data.value?.items ?? []).map((layout) => ({
    value: layout.id ?? '',
    label: layout.name,
  })),
)

// --- i18n: live keys ---------------------------------------------------------
//
// The server only knows the keys of the template it last saved
// (`GET .../i18n/keys`), so a `{% t "..." %}` just typed into the editor would
// not show up in the table until a round trip. `extractI18nKeys` (also used by
// the block editor's palette) runs the same best-effort scan over the current
// draft, and any key it finds that the server has not seen yet is merged in,
// flagged `pending` so the operator knows it is not saved.

type UsedIn = NonNullable<I18nKeyUsage['used_in']>[number]

const draftKeyUsage = computed(() => {
  const usage = new Map<string, Set<UsedIn>>()
  const parts: [UsedIn, string][] = [
    ['subject', draft.value.subject],
    ['preheader', draft.value.preheader],
    ['html', draft.value.body],
    ['text', draft.value.text],
  ]
  for (const [where, src] of parts) {
    if (!src.trim()) continue
    for (const key of extractI18nKeys(src)) {
      const set = usage.get(key) ?? new Set<UsedIn>()
      set.add(where)
      usage.set(key, set)
    }
  }
  return usage
})

const serverKeyItems = computed(() => i18nKeys.data.value?.items ?? [])
const knownKeySet = computed(() => new Set(serverKeyItems.value.map((item) => item.key)))

/** Keys the draft references that the last server load does not know about. */
const pendingKeys = computed(() =>
  [...draftKeyUsage.value.keys()].filter((key) => !knownKeySet.value.has(key)).sort(),
)
const pendingKeySet = computed(() => new Set(pendingKeys.value))

// Server items keep the order the server sent (already the key's sort order,
// since `ListTemplateI18nKeys` walks the keys sorted); pending ones are only
// appended, sorted among themselves, so a key that was already known does not
// jump around the table as new, still-unsaved keys come and go.
const mergedKeyItems = computed<I18nKeyUsage[]>(() => {
  const items = [...serverKeyItems.value]
  for (const key of pendingKeys.value) {
    items.push({ key, used_in: [...(draftKeyUsage.value.get(key) ?? [])] })
  }
  return items
})

// --- i18n: editable draft bundle ---------------------------------------------
//
// A local copy of the bundle the operator edits directly; it is only ever
// written back to the server through `saveI18n`/`persistI18nIfDirty`, never by
// the read-only reload of `i18nBundle`, so a background refresh cannot clobber
// an in-progress edit (guarded by `i18nDirty` below).

const i18nDraftBundle = ref<I18nBundle>({ locales: {} })

function cloneLocales(
  locales: NonNullable<I18nBundle['locales']> | undefined,
): NonNullable<I18nBundle['locales']> {
  const out: NonNullable<I18nBundle['locales']> = {}
  for (const [loc, kv] of Object.entries(locales ?? {})) out[loc] = { ...kv }
  return out
}

function snapshotI18n(bundle: I18nBundle): string {
  return JSON.stringify(cloneLocales(bundle.locales))
}

const i18nBaseline = ref(snapshotI18n({}))
const i18nDirty = computed(() => snapshotI18n(i18nDraftBundle.value) !== i18nBaseline.value)

// Only syncs from the server when there is nothing unsaved to lose: a reload
// triggered by something else on the page (e.g. the main form's save) must not
// silently discard translations the operator is mid-way through typing.
watch(i18nBundle.data, (bundle) => {
  if (i18nDirty.value) return
  const next: I18nBundle = { locales: cloneLocales(bundle?.locales) }
  i18nDraftBundle.value = next
  i18nBaseline.value = snapshotI18n(next)
})

// A different template entirely (navigating from one editor to another):
// nothing of the previous draft belongs here.
watch(
  () => props.templateId,
  () => {
    i18nDraftBundle.value = { locales: {} }
    i18nBaseline.value = snapshotI18n(i18nDraftBundle.value)
  },
)

const effectiveDefaultLocale = computed(
  () =>
    i18nKeys.data.value?.default_locale ||
    i18nDraftBundle.value.default_locale ||
    draft.value.default_locale ||
    'en',
)

const matrixKeys = computed<I18nKeyList>(() => ({
  default_locale: effectiveDefaultLocale.value,
  locales: i18nKeys.data.value?.locales,
  items: mergedKeyItems.value,
}))

const matrixBundle = computed<I18nBundle>(() => ({
  default_locale: effectiveDefaultLocale.value,
  locales: i18nDraftBundle.value.locales,
}))

const matrix = computed(() => buildI18nMatrix(matrixKeys.value, matrixBundle.value))
const i18nSummary = computed(() => i18nSummaryKind(matrix.value))
const knownI18nKeys = computed(() => matrix.value.rows.map((row) => row.key))

const localeSelectOptions = computed(() => {
  const values = new Set(matrix.value.locales)
  if (draft.value.default_locale) values.add(draft.value.default_locale)
  return [...values].sort().map((value) => ({ value, label: value }))
})

function ownTranslation(key: string, locale: string): string {
  return i18nDraftBundle.value.locales?.[locale]?.[key] ?? ''
}

function setTranslation(key: string, locale: string, value: string) {
  const locales = cloneLocales(i18nDraftBundle.value.locales)
  const table = { ...(locales[locale] ?? {}) }
  if (value) table[key] = value
  else delete table[key]
  locales[locale] = table
  i18nDraftBundle.value = { ...i18nDraftBundle.value, locales }
}

/** A textarea that grows a little with the content instead of always being one line. */
function textRows(value: string): number {
  if (!value) return 1
  const lines = value.split('\n').length
  const wrapped = Math.ceil(value.length / 40)
  return Math.min(8, Math.max(1, lines, wrapped))
}

// --- i18n: locales -----------------------------------------------------------

const LOCALE_TAG = /^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$/
const newLocaleInput = ref('')

const newLocaleError = computed(() => {
  const tag = newLocaleInput.value.trim()
  if (!tag) return undefined
  return LOCALE_TAG.test(tag) ? undefined : t('template.i18nLocaleInvalid')
})

function addLocale() {
  const tag = newLocaleInput.value.trim()
  if (!tag || !LOCALE_TAG.test(tag)) return
  newLocaleInput.value = ''
  if (i18nDraftBundle.value.locales?.[tag]) return
  const locales = cloneLocales(i18nDraftBundle.value.locales)
  locales[tag] = {}
  i18nDraftBundle.value = { ...i18nDraftBundle.value, locales }
}

async function removeLocale(tag: string) {
  if (!(await confirm(t('template.i18nRemoveLocaleConfirm', { locale: tag })))) return
  const locales = cloneLocales(i18nDraftBundle.value.locales)
  delete locales[tag]
  i18nDraftBundle.value = { ...i18nDraftBundle.value, locales }
}

function removeUnusedKey(key: string) {
  const locales = cloneLocales(i18nDraftBundle.value.locales)
  for (const loc of Object.keys(locales)) {
    if (!(key in locales[loc]!)) continue
    const table = { ...locales[loc] }
    delete table[key]
    locales[loc] = table
  }
  i18nDraftBundle.value = { ...i18nDraftBundle.value, locales }
}

// --- i18n: save ---------------------------------------------------------------

const savingI18n = ref(false)

async function persistI18nIfDirty() {
  if (!props.templateId || !i18nDirty.value) return
  await client.put('/api/v1/templates/{templateId}/i18n', {
    params: { path: { templateId: props.templateId } },
    body: { locales: cloneLocales(i18nDraftBundle.value.locales) },
  })
  i18nBaseline.value = snapshotI18n(i18nDraftBundle.value)
  await Promise.all([i18nBundle.reload(), i18nKeys.reload()])
}

async function saveI18n() {
  if (!props.templateId) return
  savingI18n.value = true
  try {
    await persistI18nIfDirty()
    toast.success(t('common.saved'))
  } catch (error) {
    toast.fail(error)
  } finally {
    savingI18n.value = false
  }
}

// Leaving the page (or the tab) with unsaved translations is confirmed the
// same way a mode switch is: `beforeunload` for the browser-level exit, and
// the breadcrumb link for in-app navigation (`leaveToList` below).
function onBeforeUnload(event: BeforeUnloadEvent) {
  if (!i18nDirty.value) return
  event.preventDefault()
  event.returnValue = ''
}
onMounted(() => window.addEventListener('beforeunload', onBeforeUnload))
onUnmounted(() => window.removeEventListener('beforeunload', onBeforeUnload))

async function leaveToList() {
  if (i18nDirty.value && !(await confirm(t('template.i18nUnsavedConfirm')))) return
  navigate({ name: 'templates' })
}

// --- preview ---------------------------------------------------------------
const previewLocale = ref('')
const previewVarsText = ref('{}')
const previewTenantVars = ref<TenantVars>({})
const previewRecipientText = ref('{\n  "email": "sample@example.com",\n  "name": "Sample"\n}')
const previewHtml = ref('')
const previewResult = ref<{
  locale?: string
  warnings?: string[]
  missing?: { key: string; locale: string }[]
}>({})
const previewing = ref(false)

const varsError = computed(() => tryParseJson<Vars>(previewVarsText.value, {}).error)
const recipientError = computed(
  () => tryParseJson<PreviewRecipient>(previewRecipientText.value, {}).error,
)

const localeOptions = computed(() => matrix.value.locales.map((value) => ({ value, label: value })))

async function runPreview() {
  if (!props.templateId) return
  previewing.value = true
  try {
    const result = await client.post('/api/v1/templates/{templateId}/preview', {
      params: { path: { templateId: props.templateId } },
      body: {
        ...(previewLocale.value ? { locale: previewLocale.value } : {}),
        vars: tryParseJson<Vars>(previewVarsText.value, {}).value,
        recipient: tryParseJson<PreviewRecipient>(previewRecipientText.value, {}).value,
        // Bound as `tenant` in the render, the same way a real send binds it.
        ...(Object.keys(previewTenantVars.value).length
          ? { tenant_vars: previewTenantVars.value }
          : {}),
      },
    })
    previewHtml.value = result.html
    previewResult.value = {
      locale: result.locale,
      warnings: result.warnings ?? [],
      missing: result.missing_keys ?? [],
    }
  } catch (error) {
    toast.fail(error)
  } finally {
    previewing.value = false
  }
}

// --- persistence -----------------------------------------------------------
function body() {
  return {
    name: draft.value.name,
    subject: draft.value.subject,
    mode: draft.value.mode,
    body: draft.value.body,
    ...(draft.value.preheader ? { preheader: draft.value.preheader } : {}),
    ...(draft.value.text ? { text: draft.value.text } : {}),
    ...(draft.value.layout_id ? { layout_id: draft.value.layout_id } : {}),
    ...(draft.value.default_locale ? { default_locale: draft.value.default_locale } : {}),
    ...(blocks.value ? { blocks: blocks.value } : {}),
  }
}

async function save() {
  saving.value = true
  try {
    if (props.templateId && current.value?.version !== undefined) {
      await client.put('/api/v1/templates/{templateId}', {
        params: { path: { templateId: props.templateId } },
        body: { ...body(), version: current.value.version },
      })
      // The translation draft rides along with the main save so "저장" is one
      // action from the operator's point of view, even though it is two
      // requests under the hood.
      await persistI18nIfDirty()
      await Promise.all([loaded.reload(), i18nKeys.reload()])
    } else {
      const created = await client.post('/api/v1/templates', { body: body() })
      if (created.id) navigate({ name: 'template', params: { templateId: created.id } })
    }
    toast.success(t('common.saved'))
  } catch (error) {
    toast.fail(error)
  } finally {
    saving.value = false
  }
}

async function publish() {
  if (!props.templateId) return
  if (!(await confirm(t('template.confirmPublish')))) return
  publishing.value = true
  try {
    const version = await client.post('/api/v1/templates/{templateId}/publish', {
      params: { path: { templateId: props.templateId } },
      body: { allow_missing_i18n_keys: allowMissingKeys.value },
    })
    toast.success(t('template.published', { id: version.id }))
    await loaded.reload()
  } catch (error) {
    toast.fail(error)
  } finally {
    publishing.value = false
  }
}

// --- i18n YAML -------------------------------------------------------------
const yamlInput = ref<HTMLInputElement | null>(null)

async function exportYaml() {
  if (!props.templateId) return
  try {
    const yaml = await client.getI18nYaml(props.templateId)
    downloadText(`${draft.value.name || 'template'}-i18n.yaml`, yaml, 'application/x-yaml')
  } catch (error) {
    toast.fail(error)
  }
}

async function importYaml(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file || !props.templateId) return
  if (i18nDirty.value && !(await confirm(t('template.i18nUnsavedConfirm')))) return
  try {
    const result = await client.putI18nYaml(props.templateId, await file.text())
    toast.success(t('template.imported', { keys: result.keys, locales: result.locales }))
    await Promise.all([i18nBundle.reload(), i18nKeys.reload()])
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="templateId ? draft.name || t('template.one') : t('template.new')">
      <template #breadcrumb>
        <a class="sp-link" href="#" @click.prevent="leaveToList">
          ← {{ t('template.title') }}
        </a>
      </template>
      <template #actions>
        <!--
          Content is a tenant's, and the system tenant never sends: editing and
          publishing are hidden there rather than failing on submit.
        -->
        <SpButton
          v-if="!systemTenant"
          variant="primary"
          :loading="saving"
          :disabled="!draft.name || !draft.subject"
          @click="save"
        >
          {{ t('common.save') }}
        </SpButton>
        <SpButton v-if="templateId && !systemTenant" :loading="publishing" @click="publish">
          {{ t('template.publish') }}
        </SpButton>
      </template>
    </SpPageHeader>

    <SpErrorNotice
      :error="loaded.error.value"
      :on-retry="() => loaded.reload()"
      class="sp-page__block"
    />

    <SpCard class="sp-page__block">
      <div class="sp-form-grid">
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('template.subject')" required>
          <SpInput :id="id" v-model="draft.subject" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('template.preheader')">
          <SpInput :id="id" v-model="draft.preheader" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('template.layout')">
          <SpSelect
            :id="id"
            v-model="draft.layout_id"
            :options="layoutOptions"
            :placeholder="t('common.none')"
          />
        </SpField>
      </div>
    </SpCard>

    <div class="sp-split sp-split--editor">
      <div>
        <SpCard :title="t('template.body')" :padded="false">
          <template #header>
            <SpTabs
              :model-value="draft.mode"
              :tabs="modeTabs"
              :aria-label="t('template.mode')"
              class="sp-editor__tabs"
              @update:model-value="changeMode"
            />
          </template>

          <div class="sp-editor__body">
            <SpBlockEditorSlot
              v-if="draft.mode === 'blocks'"
              v-model="blocks"
              :mjml="draft.body"
              :locale="draft.default_locale"
              :i18n-keys="knownI18nKeys"
              @update:mjml="draft.body = $event"
            >
              <slot name="block-editor" />
            </SpBlockEditorSlot>
            <SpCodeEditor
              v-else
              v-model="draft.body"
              :language="draft.mode === 'mjml' ? 'mjml' : 'html'"
              :aria-label="t('template.body')"
            />
          </div>
        </SpCard>

        <SpCard
          :title="t('template.text')"
          :subtitle="t('template.textHint')"
          class="sp-page__block"
        >
          <SpField v-slot="{ id }" :label="t('template.text')">
            <SpTextarea :id="id" v-model="draft.text" mono :rows="6" />
          </SpField>
        </SpCard>
      </div>

      <div>
        <SpCard :title="t('template.i18n')" :padded="false" class="sp-page__block">
          <template #actions>
            <SpButton
              size="sm"
              variant="primary"
              :loading="savingI18n"
              :disabled="!templateId || !i18nDirty"
              @click="saveI18n"
            >
              {{ t('template.i18nSave') }}
            </SpButton>
            <SpButton size="sm" :disabled="!templateId" @click="exportYaml">
              {{ t('template.exportYaml') }}
            </SpButton>
            <SpButton size="sm" :disabled="!templateId" @click="yamlInput?.click()">
              {{ t('template.importYaml') }}
            </SpButton>
            <input
              ref="yamlInput"
              class="sp-visually-hidden"
              type="file"
              accept=".yaml,.yml,application/x-yaml,text/yaml"
              @change="importYaml"
            />
          </template>

          <p v-if="!templateId" class="sp-note sp-i18n__note">
            {{ t('template.i18nCreateFirst') }}
          </p>

          <p v-if="i18nSummary === 'empty'" class="sp-note sp-i18n__note">
            {{ t('template.i18nNoKeys') }}
          </p>
          <p v-else-if="i18nSummary === 'complete'" class="sp-note sp-i18n__note">
            {{ t('template.i18nComplete') }}
          </p>
          <p v-else class="sp-note sp-note--warn sp-i18n__note">
            {{ t('template.i18nMissingCount', { count: matrix.missingTotal }) }}
          </p>

          <div class="sp-i18n__locales sp-i18n__note">
            <SpField v-slot="{ id }" :label="t('template.defaultLocale')" inline>
              <SpSelect
                :id="id"
                v-model="draft.default_locale"
                :options="localeSelectOptions"
                :disabled="!templateId"
              />
            </SpField>
            <SpField v-slot="{ id }" :label="t('template.i18nAddLocale')" inline>
              <SpInput
                :id="id"
                v-model="newLocaleInput"
                :placeholder="t('template.i18nLocalePlaceholder')"
                :disabled="!templateId"
                @keyup.enter="addLocale"
              />
            </SpField>
            <SpButton
              size="sm"
              :disabled="!templateId || !newLocaleInput.trim() || !!newLocaleError"
              @click="addLocale"
            >
              {{ t('template.i18nAddLocale') }}
            </SpButton>
          </div>
          <p v-if="newLocaleError" class="sp-note sp-note--warn sp-i18n__note" role="alert">
            {{ newLocaleError }}
          </p>

          <div class="sp-i18n__scroll">
            <table class="sp-table sp-i18n__table">
              <caption class="sp-visually-hidden">
                {{
                  t('template.i18n')
                }}
              </caption>
              <thead>
                <tr>
                  <th scope="col">{{ t('template.i18nKey') }}</th>
                  <th v-for="loc in matrix.locales" :key="loc" scope="col">
                    {{ loc }}
                    <button
                      v-if="loc !== effectiveDefaultLocale"
                      type="button"
                      class="sp-i18n__removelocale"
                      :disabled="!templateId"
                      :aria-label="t('template.i18nRemoveLocale', { locale: loc })"
                      @click="removeLocale(loc)"
                    >
                      ×
                    </button>
                  </th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="row in matrix.rows" :key="row.key">
                  <th scope="row" class="sp-mono sp-i18n__key">
                    {{ row.key }}
                    <span v-if="pendingKeySet.has(row.key)" class="sp-i18n__pending">
                      {{ t('template.i18nPending') }}
                    </span>
                    <span class="sp-muted sp-i18n__used">{{ row.usedIn.join(', ') }}</span>
                  </th>
                  <td
                    v-for="cell in row.cells"
                    :key="cell.locale"
                    class="sp-i18n__cell"
                    :class="{
                      'is-missing': cell.missing,
                      'is-inherited': cell.inherited,
                    }"
                    :title="cell.inherited ? `← ${cell.from}` : undefined"
                  >
                    <span v-if="cell.missing" class="sp-i18n__missing">
                      {{ t('template.i18nMissing') }}
                    </span>
                    <SpTextarea
                      :model-value="ownTranslation(row.key, cell.locale)"
                      :placeholder="cell.inherited ? cell.value : undefined"
                      :rows="textRows(ownTranslation(row.key, cell.locale) || cell.value)"
                      mono
                      :disabled="!templateId"
                      :aria-label="`${row.key} (${cell.locale})`"
                      class="sp-i18n__input"
                      @update:model-value="setTranslation(row.key, cell.locale, $event)"
                    />
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div v-if="matrix.unusedKeys.length" class="sp-i18n__unused">
            <h3 class="sp-i18n__subhead">{{ t('template.i18nUnused') }}</h3>
            <p class="sp-note">{{ t('template.i18nUnusedHint') }}</p>
            <ul class="sp-i18n__unusedlist">
              <li v-for="key in matrix.unusedKeys" :key="key">
                <span class="sp-mono">{{ key }}</span>
                <SpButton size="sm" variant="ghost" @click="removeUnusedKey(key)">
                  {{ t('common.delete') }}
                </SpButton>
              </li>
            </ul>
          </div>
        </SpCard>

        <SpCard :title="t('template.preview')">
          <template #actions>
            <SpButton
              size="sm"
              variant="primary"
              :loading="previewing"
              :disabled="!templateId"
              @click="runPreview"
            >
              {{ t('template.render') }}
            </SpButton>
          </template>

          <div class="sp-form-grid sp-form-grid--wide sp-page__block">
            <SpField v-slot="{ id }" :label="t('template.previewLocale')">
              <SpSelect
                :id="id"
                v-model="previewLocale"
                :options="localeOptions"
                :placeholder="t('common.none')"
              />
            </SpField>
            <SpField v-slot="{ id }" :label="t('template.previewVars')" :error="varsError">
              <SpTextarea :id="id" v-model="previewVarsText" mono :rows="4" />
            </SpField>
            <SpField
              v-slot="{ id }"
              :label="t('template.previewRecipient')"
              :error="recipientError"
            >
              <SpTextarea :id="id" v-model="previewRecipientText" mono :rows="5" />
            </SpField>
          </div>

          <TenantVarsEditor
            v-model="previewTenantVars"
            class="sp-page__block"
            :tenant-id="tenantId"
            :remember="false"
          />

          <p v-if="previewResult.locale" class="sp-note sp-page__block">
            {{ t('template.renderedAs', { locale: previewResult.locale }) }}
          </p>
          <ul v-if="previewResult.missing?.length" class="sp-note sp-note--warn sp-page__block">
            <li v-for="issue in previewResult.missing" :key="`${issue.key}:${issue.locale}`">
              {{ issue.key }} — {{ issue.locale }}
            </li>
          </ul>

          <!--
            `srcdoc` inside a sandboxed iframe: the rendered mail is untrusted
            markup, so it gets no same-origin access, no scripts and no ability
            to navigate the console.
          -->
          <iframe
            class="sp-preview-frame"
            sandbox=""
            referrerpolicy="no-referrer"
            :title="t('template.preview')"
            :srcdoc="previewHtml"
          />
        </SpCard>
      </div>
    </div>
  </div>
</template>

<style scoped>
.sp-editor__tabs {
  border-bottom: 0;
}

.sp-editor__body {
  padding: var(--sp-space-3);
}

.sp-i18n__note {
  margin: var(--sp-space-3);
}

.sp-i18n__locales {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: var(--sp-space-3);
}

.sp-i18n__scroll {
  max-height: 420px;
  overflow: auto;
}

.sp-i18n__table {
  width: 100%;
  border-collapse: collapse;
}

.sp-i18n__table th,
.sp-i18n__table td {
  padding: var(--sp-space-1) var(--sp-space-2);
  font-size: var(--sp-font-size-sm);
  text-align: start;
  vertical-align: top;
  border-bottom: 1px solid var(--sp-border);
}

.sp-i18n__table thead th {
  position: sticky;
  top: 0;
  background: var(--sp-surface-alt);
}

.sp-i18n__removelocale {
  margin-inline-start: var(--sp-space-1);
  color: var(--sp-text-muted);
  cursor: pointer;
  background: none;
  border: 0;
}

.sp-i18n__removelocale:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}

.sp-i18n__key {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-weight: 500;
}

.sp-i18n__used {
  font-family: var(--sp-font);
  font-size: 11px;
}

.sp-i18n__pending {
  color: var(--sp-text-muted);
  font-weight: 400;
  font-style: italic;
}

.sp-i18n__cell {
  max-width: 260px;
}

.sp-i18n__input {
  min-height: unset;
}

.sp-i18n__cell.is-missing {
  background: var(--sp-danger-soft);
}

.sp-i18n__cell.is-inherited .sp-i18n__input {
  color: var(--sp-text-muted);
}

.sp-i18n__missing {
  display: block;
  margin-bottom: 2px;
  color: var(--sp-danger);
  font-weight: 600;
}

.sp-i18n__unused {
  margin: var(--sp-space-3);
  padding-top: var(--sp-space-3);
  border-top: 1px solid var(--sp-border);
}

.sp-i18n__subhead {
  margin: 0 0 var(--sp-space-1);
  font-size: var(--sp-font-size);
}

.sp-i18n__unusedlist {
  margin: 0;
  padding: 0;
  list-style: none;
}

.sp-i18n__unusedlist li {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-space-2);
  padding: var(--sp-space-1) 0;
}
</style>
