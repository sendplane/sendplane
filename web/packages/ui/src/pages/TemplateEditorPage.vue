<script setup lang="ts">
import type {
  ContentMode,
  I18nBundle,
  PreviewRecipient,
  Template,
  TenantVars,
  Vars,
} from '@sendplane/api'
import { computed, defineAsyncComponent, ref, watch } from 'vue'

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
import { buildI18nMatrix } from '../lib/i18n-matrix.js'

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

const matrix = computed(() => buildI18nMatrix(i18nKeys.data.value, i18nBundle.data.value))

const modeTabs = computed<TabItem[]>(() =>
  (['blocks', 'mjml', 'html'] as const).map((mode) => ({
    value: mode,
    label: t(`template.modes.${mode}`),
  })),
)

const knownI18nKeys = computed(() => (i18nKeys.data.value?.items ?? []).map((item) => item.key))

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
        <a class="sp-link" href="#" @click.prevent="navigate({ name: 'templates' })">
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
        <SpField v-slot="{ id }" :label="t('template.defaultLocale')">
          <SpInput :id="id" v-model="draft.default_locale" />
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

          <p v-if="matrix.missingTotal === 0" class="sp-note sp-i18n__note">
            {{ t('template.i18nComplete') }}
          </p>
          <p v-else class="sp-note sp-note--warn sp-i18n__note">
            {{ t('template.i18nMissingCount', { count: matrix.missingTotal }) }}
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
                  <th v-for="loc in matrix.locales" :key="loc" scope="col">{{ loc }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="row in matrix.rows" :key="row.key">
                  <th scope="row" class="sp-mono sp-i18n__key">
                    {{ row.key }}
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
                    <span v-else>{{ cell.value }}</span>
                  </td>
                </tr>
              </tbody>
            </table>
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

.sp-i18n__cell {
  max-width: 220px;
  overflow-wrap: anywhere;
}

.sp-i18n__cell.is-missing {
  background: var(--sp-danger-soft);
}

.sp-i18n__cell.is-inherited {
  color: var(--sp-text-muted);
  font-style: italic;
}

.sp-i18n__missing {
  color: var(--sp-danger);
  font-weight: 600;
}
</style>
