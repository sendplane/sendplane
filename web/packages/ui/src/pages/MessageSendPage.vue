<script setup lang="ts">
import type { MessageRecipient, MessageResult, Sender, TenantVars, Vars } from '@sendplane/api'
import { computed, ref } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpCheckbox from '../components/SpCheckbox.vue'
import SpEmptyState from '../components/SpEmptyState.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpLink from '../components/SpLink.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import SpSharedBadge from '../components/SpSharedBadge.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTextarea from '../components/SpTextarea.vue'
import TenantVarsEditor from '../components/TenantVarsEditor.vue'
import { useApiToast } from '../composables/useApiToast.js'
import { useAsync } from '../composables/useAsync.js'
import { useSendplane } from '../context.js'
import { templateOption, templateReference, templateUseBlocked } from '../lib/content.js'
import { safeJson, shortId, tryParseJson } from '../lib/format.js'
import {
  isSenderUseDenied,
  isTemplateUseDenied,
  isTenantVarsMissing,
  looksTemplated,
  missingTenantVarKeys,
} from '../lib/platform.js'

/** The spec's `MessageRequest.to` cap; more than this is two requests. */
const MAX_RECIPIENTS = 1000

const { client, t, navigate, systemTenant, tenantId } = useSendplane()
const toast = useApiToast()

interface RecipientRow {
  id: number
  email: string
  name: string
  locale: string
  varsText: string
}

interface HeaderRow {
  id: number
  key: string
  value: string
}

let nextId = 1

const senderId = ref('')
const templateId = ref('')
// Name the template by key (`template_key`) rather than by ID (ADR-0018).
const byKey = ref(false)
const defaultLocale = ref('')
const varsText = ref('{}')
const tenantVars = ref<TenantVars>({})
const recipients = ref<RecipientRow[]>([
  { id: nextId++, email: '', name: '', locale: '', varsText: '' },
])
const pasteText = ref('')
const headers = ref<HeaderRow[]>([])
const idempotencyKey = ref(newIdempotencyKey())

const sending = ref(false)
const previewing = ref(false)
const previewHtml = ref('')
const previewLocale = ref('')
const result = ref<MessageResult | undefined>()
// Errors that belong next to a field rather than in a toast: the operator has
// to change the very input they are about.
const missingTenantVars = ref<string[]>([])
const senderUseError = ref('')
const templateUseError = ref('')

const senders = useAsync((signal) =>
  client.get('/api/v1/senders', { params: { query: { limit: 200 } }, signal }),
)
const templates = useAsync((signal) =>
  client.get('/api/v1/templates', { params: { query: { limit: 200 } }, signal }),
)

const senderList = computed<Sender[]>(() => senders.data.value?.items ?? [])

const senderOptions = computed(() =>
  senderList.value.map((sender) => ({
    value: sender.id ?? '',
    label: sender.shared
      ? `${sender.name} <${sender.from_email}> — ${t('shared.badge')}${usesSuffix(sender)}`
      : `${sender.name} <${sender.from_email}>`,
  })),
)

function usesSuffix(sender: Sender): string {
  return sender.uses?.length ? ` (${sender.uses.join(', ')})` : ''
}

const selectedSender = computed(() => senderList.value.find((s) => s.id === senderId.value))

/**
 * `uses` is only present on a shared sender, and an absent one means "anything"
 * (the spec): a tenant's own sender is never restricted. The server is still
 * the authority — `403 sender_use_denied` — this only avoids a pointless round
 * trip and explains why.
 */
const senderAllowsTransactional = computed(() => {
  const uses = selectedSender.value?.uses
  return !uses || uses.includes('transactional')
})

/**
 * A shared sender's From fields are Liquid over `tenant_vars`, so they are what
 * the approximate render in the editor is for. A tenant's own sender has
 * literal addresses and gets no render pane.
 */
const senderTemplates = computed(() => {
  const sender = selectedSender.value
  if (!sender?.shared) return undefined
  if (!looksTemplated(sender.from_name) && !looksTemplated(sender.from_email)) return undefined
  return {
    ...(sender.from_name ? { from_name: sender.from_name } : {}),
    from_email: sender.from_email,
  }
})

/**
 * Only a published template can be sent; the send would 422 otherwise. The
 * list holds the tenant's own templates and the shared ones (ADR-0018); a
 * shared one restricted to campaigns stays listed but disabled, so the
 * operator sees why it cannot be picked.
 */
const templateOptions = computed(() =>
  (templates.data.value?.items ?? [])
    .filter((template) => Boolean(template.published_version_id))
    .map((template) =>
      templateOption(template, {
        use: 'transactional',
        byKey: byKey.value,
        t,
        detail: shortId(template.published_version_id),
      }),
    ),
)

const selectedTemplate = computed(() =>
  (templates.data.value?.items ?? []).find((template) => template.id === templateId.value),
)

const templateBlocked = computed(
  () => !!selectedTemplate.value && templateUseBlocked(selectedTemplate.value, 'transactional'),
)

/** Selecting by key needs a template that has one. */
const templateKeyMissing = computed(
  () => byKey.value && !!selectedTemplate.value && !selectedTemplate.value.key,
)

const templateError = computed(() => {
  if (templateBlocked.value) {
    return t('content.templateUseBlocked', {
      uses: (selectedTemplate.value?.uses ?? []).join(', '),
    })
  }
  if (templateKeyMissing.value) return t('content.selectedHasNoKey')
  return templateUseError.value || undefined
})

const varsError = computed(() => tryParseJson<Vars>(varsText.value, {}).error)

const rowVarsErrors = computed(() =>
  Object.fromEntries(
    recipients.value.map((row) => [row.id, tryParseJson<Vars>(row.varsText, {}).error]),
  ),
)

const filledRecipients = computed(() => recipients.value.filter((row) => row.email.trim()))

const tooManyRecipients = computed(() => filledRecipients.value.length > MAX_RECIPIENTS)

const canSend = computed(
  () =>
    Boolean(senderId.value) &&
    Boolean(templateId.value) &&
    filledRecipients.value.length > 0 &&
    !tooManyRecipients.value &&
    !varsError.value &&
    senderAllowsTransactional.value &&
    !templateBlocked.value &&
    !templateKeyMissing.value &&
    Object.values(rowVarsErrors.value).every((error) => !error),
)

function addRecipient() {
  recipients.value = [
    ...recipients.value,
    { id: nextId++, email: '', name: '', locale: '', varsText: '' },
  ]
}

function removeRecipient(id: number) {
  const next = recipients.value.filter((row) => row.id !== id)
  recipients.value = next.length
    ? next
    : [{ id: nextId++, email: '', name: '', locale: '', varsText: '' }]
}

/**
 * `email[,name[,locale]]` per line, which is what an operator has in a ticket.
 * It appends rather than replaces, and stops at the cap instead of silently
 * dropping the tail.
 */
function applyPaste() {
  const parsed: RecipientRow[] = []
  for (const line of pasteText.value.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    const [email = '', name = '', locale = ''] = trimmed.split(',').map((part) => part.trim())
    if (!email) continue
    parsed.push({ id: nextId++, email, name, locale, varsText: '' })
  }
  if (parsed.length === 0) return
  const kept = recipients.value.filter((row) => row.email.trim())
  const merged = [...kept, ...parsed]
  if (merged.length > MAX_RECIPIENTS) {
    toast.push(t('message.pasteTruncated', { max: MAX_RECIPIENTS }), 'warning')
  }
  recipients.value = merged.slice(0, MAX_RECIPIENTS)
  pasteText.value = ''
}

function addHeader() {
  headers.value = [...headers.value, { id: nextId++, key: '', value: '' }]
}

function removeHeader(id: number) {
  headers.value = headers.value.filter((row) => row.id !== id)
}

function regenerateKey() {
  idempotencyKey.value = newIdempotencyKey()
}

function toRecipient(row: RecipientRow): MessageRecipient {
  const vars = tryParseJson<Vars>(row.varsText, {}).value
  return {
    email: row.email.trim(),
    ...(row.name.trim() ? { name: row.name.trim() } : {}),
    ...(row.locale.trim() ? { locale: row.locale.trim() } : {}),
    ...(Object.keys(vars).length ? { vars } : {}),
  }
}

function headerRecord(): Record<string, string> | undefined {
  const out: Record<string, string> = {}
  for (const row of headers.value) {
    const key = row.key.trim()
    if (key) out[key] = row.value
  }
  return Object.keys(out).length ? out : undefined
}

function requestBody() {
  const vars = tryParseJson<Vars>(varsText.value, {}).value
  const extraHeaders = headerRecord()
  return {
    ...templateReference(selectedTemplate.value, templateId.value, byKey.value),
    sender_id: senderId.value,
    to: filledRecipients.value.map(toRecipient),
    ...(Object.keys(tenantVars.value).length ? { tenant_vars: tenantVars.value } : {}),
    ...(Object.keys(vars).length ? { vars } : {}),
    ...(defaultLocale.value ? { default_locale: defaultLocale.value } : {}),
    ...(extraHeaders ? { headers: extraHeaders } : {}),
  }
}

function clearFieldErrors() {
  missingTenantVars.value = []
  senderUseError.value = ''
  templateUseError.value = ''
}

/**
 * The two failures that name an input get rendered on it; everything else is a
 * toast, because there is no field to point at.
 */
function handleSendError(error: unknown) {
  if (isTenantVarsMissing(error)) {
    const keys = missingTenantVarKeys(error)
    missingTenantVars.value = keys
    if (keys.length === 0) toast.fail(error)
    return
  }
  if (isSenderUseDenied(error)) {
    senderUseError.value = error instanceof Error ? error.message : String(error)
    return
  }
  if (isTemplateUseDenied(error)) {
    templateUseError.value = error instanceof Error ? error.message : String(error)
    return
  }
  toast.fail(error)
}

async function send() {
  if (!canSend.value) return
  clearFieldErrors()
  sending.value = true
  try {
    result.value = await client.post('/api/v1/messages', {
      params: { header: { 'Idempotency-Key': idempotencyKey.value } },
      body: requestBody(),
    })
    toast.success(t('message.sent', { count: result.value.deliveries.length }))
    // A new key, so the next send is a new send rather than a replay of this one.
    regenerateKey()
  } catch (error) {
    handleSendError(error)
  } finally {
    sending.value = false
  }
}

/** The same render path a send uses, against the first recipient (spec §6.3). */
async function preview() {
  const template = templateId.value
  const first = filledRecipients.value[0]
  if (!template || !first) return
  clearFieldErrors()
  previewing.value = true
  try {
    const rendered = await client.post('/api/v1/templates/{templateId}/preview', {
      params: { path: { templateId: template } },
      body: {
        ...(first.locale.trim() ? { locale: first.locale.trim() } : {}),
        recipient: toRecipient(first),
        ...(Object.keys(tenantVars.value).length ? { tenant_vars: tenantVars.value } : {}),
        vars: tryParseJson<Vars>(varsText.value, {}).value,
      },
    })
    previewHtml.value = rendered.html
    previewLocale.value = rendered.locale
  } catch (error) {
    handleSendError(error)
  } finally {
    previewing.value = false
  }
}

function newIdempotencyKey(): string {
  const webcrypto = globalThis.crypto
  if (webcrypto && typeof webcrypto.randomUUID === 'function') return webcrypto.randomUUID()
  // happy-dom and older browsers: still unique enough to be a replay key.
  return `sp-${Date.now().toString(16)}-${Math.random().toString(16).slice(2, 10)}`
}

const senderHint = computed(() => {
  if (!selectedSender.value?.shared) return undefined
  const uses = selectedSender.value.uses
  return uses?.length ? t('message.senderUses', { uses: uses.join(', ') }) : t('shared.hint')
})

const exampleVars = safeJson({ order: { total: 1200 } }, 0)
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('message.title')" :subtitle="t('message.hint')">
      <template #actions>
        <SpButton
          :loading="previewing"
          :disabled="!templateId || filledRecipients.length === 0"
          @click="preview"
        >
          {{ t('message.preview') }}
        </SpButton>
        <SpButton variant="primary" :loading="sending" :disabled="!canSend" @click="send">
          {{ t('message.send') }}
        </SpButton>
      </template>
    </SpPageHeader>

    <!--
      The system tenant is the operator's read-only view of platform resources
      and cannot send at all (ADR-0017), so the form is not offered there.
    -->
    <SpCard v-if="systemTenant" class="sp-page__block">
      <SpEmptyState :title="t('tenant.systemNoSend')" :description="t('tenant.systemNoSendHint')" />
    </SpCard>

    <template v-else>
      <SpErrorNotice
        :error="senders.error.value ?? templates.error.value"
        :on-retry="
          () => {
            void senders.reload()
            void templates.reload()
          }
        "
        class="sp-page__block"
      />

      <SpCard :title="t('message.message')" class="sp-page__block">
        <div class="sp-form-grid">
          <SpField
            v-slot="{ id, describedBy }"
            :label="t('campaign.sender')"
            :hint="senderHint"
            :error="
              !senderAllowsTransactional
                ? t('message.senderNotTransactional')
                : senderUseError || undefined
            "
            required
          >
            <SpSelect
              :id="id"
              v-model="senderId"
              :options="senderOptions"
              :placeholder="t('common.none')"
              :described-by="describedBy"
            />
          </SpField>
          <SpField
            v-slot="{ id, describedBy }"
            :label="t('template.one')"
            :hint="t('message.templateHint')"
            :error="templateError"
            required
          >
            <SpSelect
              :id="id"
              v-model="templateId"
              :options="templateOptions"
              :placeholder="t('common.none')"
              :described-by="describedBy"
            />
          </SpField>
          <SpCheckbox
            v-model="byKey"
            :label="t('content.selectByKey')"
            :hint="t('content.selectByKeyHint')"
          />
          <SpField v-slot="{ id }" :label="t('campaign.defaultLocale')">
            <SpInput :id="id" v-model="defaultLocale" placeholder="en" />
          </SpField>
          <SpField
            v-slot="{ id, describedBy }"
            :label="t('message.idempotencyKey')"
            :hint="t('message.idempotencyKeyHint')"
          >
            <div class="sp-actions-row">
              <SpInput :id="id" v-model="idempotencyKey" :described-by="describedBy" />
              <SpButton size="sm" @click="regenerateKey">{{ t('message.newKey') }}</SpButton>
            </div>
          </SpField>
          <SpField
            v-slot="{ id, describedBy }"
            :label="t('message.vars')"
            :hint="t('message.varsHint')"
            :error="varsError"
          >
            <SpTextarea
              :id="id"
              v-model="varsText"
              mono
              :rows="4"
              :placeholder="exampleVars"
              :described-by="describedBy"
            />
          </SpField>
        </div>
        <p v-if="selectedSender?.shared" class="sp-note">
          <SpSharedBadge /> <span class="sp-mono">{{ selectedSender.from_email }}</span>
        </p>
        <p v-if="selectedTemplate" class="sp-note">
          {{ t('template.publishedVersion') }}:
          <span class="sp-mono">{{ selectedTemplate.published_version_id }}</span>
        </p>
      </SpCard>

      <TenantVarsEditor
        v-model="tenantVars"
        class="sp-page__block"
        :tenant-id="tenantId"
        :missing-keys="missingTenantVars"
        :sender-templates="senderTemplates"
      />

      <SpCard :title="t('message.recipients')" class="sp-page__block">
        <template #actions>
          <SpButton size="sm" @click="addRecipient">{{ t('message.addRecipient') }}</SpButton>
        </template>

        <p class="sp-note">
          {{ t('message.recipientCount', { count: filledRecipients.length, max: MAX_RECIPIENTS }) }}
        </p>
        <p v-if="tooManyRecipients" class="sp-note sp-note--warn" role="alert">
          {{ t('message.tooMany', { max: MAX_RECIPIENTS }) }}
        </p>

        <table class="sp-send__table">
          <caption class="sp-visually-hidden">
            {{
              t('message.recipients')
            }}
          </caption>
          <thead>
            <tr>
              <th scope="col">{{ t('common.email') }}</th>
              <th scope="col">{{ t('common.name') }}</th>
              <th scope="col">{{ t('common.locale') }}</th>
              <th scope="col">{{ t('message.recipientVars') }}</th>
              <th scope="col">
                <span class="sp-visually-hidden">{{ t('common.actions') }}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in recipients" :key="row.id">
              <td><SpInput v-model="row.email" type="email" :aria-label="t('common.email')" /></td>
              <td><SpInput v-model="row.name" :aria-label="t('common.name')" /></td>
              <td><SpInput v-model="row.locale" :aria-label="t('common.locale')" /></td>
              <td>
                <SpInput
                  v-model="row.varsText"
                  :aria-label="t('message.recipientVars')"
                  placeholder="{}"
                />
                <p v-if="rowVarsErrors[row.id]" class="sp-note sp-note--warn" role="alert">
                  {{ rowVarsErrors[row.id] }}
                </p>
              </td>
              <td class="sp-send__rowactions">
                <SpButton size="sm" variant="ghost" @click="removeRecipient(row.id)">
                  {{ t('common.delete') }}
                </SpButton>
              </td>
            </tr>
          </tbody>
        </table>

        <SpField
          v-slot="{ id, describedBy }"
          :label="t('message.paste')"
          :hint="t('message.pasteHint')"
          class="sp-send__paste"
        >
          <SpTextarea
            :id="id"
            v-model="pasteText"
            mono
            :rows="4"
            :described-by="describedBy"
            placeholder="a@example.com,A,ko"
          />
        </SpField>
        <SpButton size="sm" :disabled="!pasteText.trim()" @click="applyPaste">
          {{ t('message.applyPaste') }}
        </SpButton>
      </SpCard>

      <SpCard
        :title="t('message.headers')"
        :subtitle="t('message.headersHint')"
        class="sp-page__block"
      >
        <template #actions>
          <SpButton size="sm" @click="addHeader">{{ t('message.addHeader') }}</SpButton>
        </template>
        <SpEmptyState v-if="headers.length === 0" :title="t('message.noHeaders')" />
        <table v-else class="sp-send__table">
          <caption class="sp-visually-hidden">
            {{
              t('message.headers')
            }}
          </caption>
          <thead>
            <tr>
              <th scope="col">{{ t('message.headerName') }}</th>
              <th scope="col">{{ t('message.headerValue') }}</th>
              <th scope="col">
                <span class="sp-visually-hidden">{{ t('common.actions') }}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="row in headers" :key="row.id">
              <td><SpInput v-model="row.key" :aria-label="t('message.headerName')" /></td>
              <td><SpInput v-model="row.value" :aria-label="t('message.headerValue')" /></td>
              <td class="sp-send__rowactions">
                <SpButton size="sm" variant="ghost" @click="removeHeader(row.id)">
                  {{ t('common.delete') }}
                </SpButton>
              </td>
            </tr>
          </tbody>
        </table>
      </SpCard>

      <div class="sp-split sp-split--halves">
        <SpCard :title="t('message.result')">
          <SpEmptyState v-if="!result" :title="t('message.noResult')" />
          <template v-else>
            <p v-if="result.idempotent_replay" class="sp-note sp-note--warn">
              {{ t('message.replay') }}
            </p>
            <p class="sp-note">
              {{ t('campaign.messageVersion') }}:
              <span class="sp-mono">{{ result.version_id }}</span>
            </p>
            <ul class="sp-send__results">
              <li v-for="item in result.deliveries" :key="item.delivery_id">
                <SpStatusBadge kind="delivery" :value="item.status" />
                <span class="sp-mono">{{ item.email }}</span>
                <SpLink :to="{ name: 'delivery', params: { deliveryId: item.delivery_id } }">
                  {{ shortId(item.delivery_id) }}
                </SpLink>
              </li>
            </ul>
            <SpButton size="sm" @click="navigate({ name: 'deliveries' })">
              {{ t('campaign.viewDeliveries') }}
            </SpButton>
          </template>
        </SpCard>

        <SpCard :title="t('message.preview')">
          <p v-if="previewLocale" class="sp-note">
            {{ t('template.renderedAs', { locale: previewLocale }) }}
          </p>
          <!--
            The rendered mail is untrusted markup, so it goes into a sandboxed
            `srcdoc` iframe with no scripts and no same-origin access, exactly
            as on the template editor.
          -->
          <iframe
            class="sp-preview-frame"
            sandbox=""
            referrerpolicy="no-referrer"
            :title="t('message.preview')"
            :srcdoc="previewHtml"
          />
        </SpCard>
      </div>
    </template>
  </div>
</template>

<style scoped>
.sp-send__table {
  width: 100%;
  margin: var(--sp-space-2) 0;
  border-collapse: collapse;
}

.sp-send__table th {
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
  font-weight: 600;
  text-align: start;
}

.sp-send__table td {
  padding: 2px var(--sp-space-1) 2px 0;
  vertical-align: top;
}

.sp-send__rowactions {
  width: 1%;
  white-space: nowrap;
}

.sp-send__paste {
  margin-top: var(--sp-space-3);
}

.sp-send__results {
  display: flex;
  flex-direction: column;
  gap: var(--sp-space-1);
  margin: var(--sp-space-2) 0;
  padding: 0;
  list-style: none;
}

.sp-send__results li {
  display: flex;
  gap: var(--sp-space-2);
  align-items: center;
}
</style>
