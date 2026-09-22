<script setup lang="ts">
import type { TenantVars } from '@sendplane/api'
import { computed, ref, watch } from 'vue'

import { useSendplane } from '../context.js'
import { safeJson, tryParseJson } from '../lib/format.js'
import { renderTenantTemplate } from '../lib/platform.js'
import SpButton from './SpButton.vue'
import SpCard from './SpCard.vue'
import SpInput from './SpInput.vue'
import SpTextarea from './SpTextarea.vue'

/**
 * The `tenant_vars` of a campaign, a transactional send or a preview.
 *
 * sendplane has no tenant registry (ADR-0006): the attributes a shared sender's
 * From templates read travel with the request, and the host's `TenantVars` hook
 * validates and substitutes them. So this is an editor for arbitrary keys, with
 * a JSON escape hatch for anything a flat table cannot hold, and it remembers
 * the last values per tenant because the same three keys get retyped all day.
 */
const props = withDefaults(
  defineProps<{
    modelValue: TenantVars
    /** Scopes the remembered values; switching tenant must not carry them over. */
    tenantId?: string
    /** Keys a `422 tenant_vars_missing` named, highlighted as the cause. */
    missingKeys?: string[]
    /**
     * A shared sender's From templates, for the approximate render below. Leave
     * unset for a tenant's own sender, which has literal addresses.
     */
    senderTemplates?: { from_name?: string; from_email?: string }
    /** Off for a preview pane, where remembering values across pages surprises. */
    remember?: boolean
  }>(),
  { remember: true },
)

const emit = defineEmits<{ 'update:modelValue': [TenantVars] }>()

const { t } = useSendplane()

interface Row {
  id: number
  key: string
  value: string
}

let nextId = 1
const rows = ref<Row[]>([])
const jsonMode = ref(false)
const jsonText = ref('{}')
// What this component last emitted, so an echo of its own value does not
// rebuild the rows and move the caret while someone is typing.
let lastEmitted = ''

function toRows(vars: TenantVars): Row[] {
  return Object.entries(vars).map(([key, value]) => ({
    id: nextId++,
    key,
    value: typeof value === 'string' ? value : safeJson(value, 0),
  }))
}

/** A table value is text unless it parses as JSON, so `plan: 3` stays a number. */
function toVars(list: Row[]): TenantVars {
  const out: TenantVars = {}
  for (const row of list) {
    const key = row.key.trim()
    if (!key) continue
    out[key] = coerce(row.value)
  }
  return out
}

function coerce(value: string): unknown {
  const trimmed = value.trim()
  if (!trimmed) return ''
  if (/^(true|false|null)$/.test(trimmed) || /^-?\d+(\.\d+)?$/.test(trimmed)) {
    return JSON.parse(trimmed)
  }
  if (trimmed.startsWith('{') || trimmed.startsWith('[')) {
    const parsed = tryParseJson<unknown>(trimmed, value)
    return parsed.error ? value : parsed.value
  }
  return value
}

watch(
  () => props.modelValue,
  (vars) => {
    const serialized = safeJson(vars, 0)
    if (serialized === lastEmitted) return
    rows.value = toRows(vars ?? {})
    jsonText.value = safeJson(vars ?? {})
  },
  { immediate: true, deep: true },
)

function push(vars: TenantVars) {
  lastEmitted = safeJson(vars, 0)
  emit('update:modelValue', vars)
  if (props.remember) storeVars(vars)
}

function syncFromRows() {
  push(toVars(rows.value))
}

const jsonError = computed(() =>
  jsonMode.value ? tryParseJson<TenantVars>(jsonText.value, {}).error : undefined,
)

function syncFromJson() {
  const parsed = tryParseJson<TenantVars>(jsonText.value, {})
  if (parsed.error) return
  const vars = parsed.value && typeof parsed.value === 'object' ? parsed.value : {}
  rows.value = toRows(vars)
  push(vars)
}

function addRow() {
  rows.value = [...rows.value, { id: nextId++, key: '', value: '' }]
}

function removeRow(id: number) {
  rows.value = rows.value.filter((row) => row.id !== id)
  syncFromRows()
}

function toggleJson() {
  if (jsonMode.value) {
    syncFromJson()
    jsonMode.value = false
  } else {
    jsonText.value = safeJson(toVars(rows.value))
    jsonMode.value = true
  }
}

// --- remembered values -----------------------------------------------------

function storageKey(): string {
  return `sendplane.tenantVars.${props.tenantId || 'default'}`
}

// `localStorage` rather than the session: these are the tenant's own
// attributes, they are not a credential, and retyping them every morning is
// the kind of friction an ops console should not have. Reads and writes are
// guarded because storage throws in a private window.
function storeVars(vars: TenantVars) {
  try {
    if (Object.keys(vars).length === 0) localStorage.removeItem(storageKey())
    else localStorage.setItem(storageKey(), JSON.stringify(vars))
  } catch {
    /* ignore */
  }
}

function recall(): TenantVars | undefined {
  try {
    const raw = localStorage.getItem(storageKey())
    if (!raw) return undefined
    const parsed = JSON.parse(raw) as unknown
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return undefined
    return Object.keys(parsed).length ? (parsed as TenantVars) : undefined
  } catch {
    return undefined
  }
}

const recalled = ref(false)

// Only ever fills an empty editor: a caller that loaded a campaign's own
// `tenant_vars` must keep them.
watch(
  () => [props.tenantId, props.remember] as const,
  () => {
    if (!props.remember || recalled.value) return
    if (Object.keys(props.modelValue ?? {}).length > 0) return
    const stored = recall()
    if (!stored) return
    recalled.value = true
    rows.value = toRows(stored)
    jsonText.value = safeJson(stored)
    push(stored)
  },
  { immediate: true },
)

function clearRemembered() {
  recalled.value = true
  rows.value = []
  jsonText.value = '{}'
  push({})
}

// --- approximate From render ----------------------------------------------

const rendered = computed(() => {
  const templates = props.senderTemplates
  if (!templates) return undefined
  const vars = jsonMode.value
    ? tryParseJson<TenantVars>(jsonText.value, {}).value
    : toVars(rows.value)
  const name = renderTenantTemplate(templates.from_name, vars)
  const email = renderTenantTemplate(templates.from_email, vars)
  return {
    name: name.text,
    email: email.text,
    missing: [...new Set([...name.missing, ...email.missing])],
  }
})

const missing = computed(() => props.missingKeys ?? [])

function isMissing(key: string): boolean {
  return missing.value.includes(key.trim())
}
</script>

<template>
  <SpCard :title="t('tenantVars.title')" :subtitle="t('tenantVars.hint')">
    <template #actions>
      <SpButton size="sm" @click="toggleJson">
        {{ jsonMode ? t('tenantVars.tableMode') : t('tenantVars.jsonMode') }}
      </SpButton>
      <SpButton size="sm" variant="ghost" @click="clearRemembered">
        {{ t('common.clear') }}
      </SpButton>
    </template>

    <p v-if="missing.length" class="sp-note sp-note--warn sp-tenant-vars__missing" role="alert">
      {{ t('tenantVars.missing', { keys: missing.join(', ') }) }}
    </p>

    <template v-if="jsonMode">
      <SpTextarea
        v-model="jsonText"
        mono
        :rows="6"
        :aria-label="t('tenantVars.json')"
        @update:model-value="syncFromJson"
      />
      <p v-if="jsonError" class="sp-note sp-note--warn" role="alert">{{ jsonError }}</p>
    </template>

    <template v-else>
      <table class="sp-tenant-vars__table">
        <caption class="sp-visually-hidden">
          {{
            t('tenantVars.title')
          }}
        </caption>
        <thead>
          <tr>
            <th scope="col">{{ t('tenantVars.key') }}</th>
            <th scope="col">{{ t('tenantVars.value') }}</th>
            <th scope="col">
              <span class="sp-visually-hidden">{{ t('common.actions') }}</span>
            </th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="row in rows" :key="row.id" :class="{ 'is-missing': isMissing(row.key) }">
            <td>
              <SpInput
                v-model="row.key"
                :aria-label="t('tenantVars.key')"
                @update:model-value="syncFromRows"
              />
            </td>
            <td>
              <SpInput
                v-model="row.value"
                :aria-label="t('tenantVars.value')"
                @update:model-value="syncFromRows"
              />
            </td>
            <td class="sp-tenant-vars__rowactions">
              <SpButton size="sm" variant="ghost" @click="removeRow(row.id)">
                {{ t('common.delete') }}
              </SpButton>
            </td>
          </tr>
        </tbody>
      </table>
      <SpButton size="sm" @click="addRow">{{ t('tenantVars.add') }}</SpButton>
    </template>

    <div v-if="rendered" class="sp-tenant-vars__render">
      <h3 class="sp-tenant-vars__subhead">{{ t('tenantVars.rendered') }}</h3>
      <p class="sp-note">{{ t('tenantVars.renderedHint') }}</p>
      <dl class="sp-detail-list">
        <dt>{{ t('sender.fromName') }}</dt>
        <dd class="sp-mono">{{ rendered.name || '—' }}</dd>
        <dt>{{ t('sender.fromEmail') }}</dt>
        <dd class="sp-mono">{{ rendered.email || '—' }}</dd>
      </dl>
      <p v-if="rendered.missing.length" class="sp-note sp-note--warn">
        {{ t('tenantVars.renderedMissing', { keys: rendered.missing.join(', ') }) }}
      </p>
    </div>
  </SpCard>
</template>

<style scoped>
.sp-tenant-vars__table {
  width: 100%;
  margin-bottom: var(--sp-space-2);
  border-collapse: collapse;
}

.sp-tenant-vars__table th {
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
  font-weight: 600;
  text-align: start;
}

.sp-tenant-vars__table td {
  padding: 2px var(--sp-space-1) 2px 0;
  vertical-align: middle;
}

.sp-tenant-vars__table tr.is-missing td {
  background: var(--sp-danger-soft);
}

.sp-tenant-vars__rowactions {
  width: 1%;
  white-space: nowrap;
}

.sp-tenant-vars__missing {
  margin: 0 0 var(--sp-space-2);
}

.sp-tenant-vars__render {
  margin-top: var(--sp-space-4);
  padding-top: var(--sp-space-3);
  border-top: 1px solid var(--sp-border);
}

.sp-tenant-vars__subhead {
  margin: 0 0 var(--sp-space-1);
  font-size: var(--sp-font-size);
}
</style>
