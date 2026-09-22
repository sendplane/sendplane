<script setup lang="ts">
import type { components, ProbeMailbox, TLSMode } from '@sendplane/api'
import { computed, ref } from 'vue'

import MailboxHealthBadge from '../components/MailboxHealthBadge.vue'
import MailboxTestPanel from '../components/MailboxTestPanel.vue'
import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpCheckbox from '../components/SpCheckbox.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import SpSharedBadge from '../components/SpSharedBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useApiToast } from '../composables/useApiToast.js'
import { useConfirm } from '../composables/useConfirm.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useSendplane } from '../context.js'
import { formatDateTime } from '../lib/format.js'

type MailboxTestResult = components['schemas']['MailboxTestResult']
type ProbeMailboxKind = components['schemas']['ProbeMailboxKind']

const { client, t, locale } = useSendplane()
const toast = useApiToast()
const confirm = useConfirm()

const list = useCursorList<ProbeMailbox>((params, signal) =>
  client.get('/api/v1/probe-mailboxes', { params: { query: params }, signal }),
)

const editing = ref<ProbeMailbox | null>(null)
const saving = ref(false)
const draft = ref({
  name: '',
  address: '',
  kind: 'imap' as ProbeMailboxKind,
  host: '',
  port: 993,
  tls: 'tls' as TLSMode,
  username: '',
  password: '',
  inbox_folder: 'INBOX',
  spam_folder: '',
  authserv_id: '',
  enabled: true,
})

// Result of the create/edit form's own "Test" button. Cleared whenever the
// form is reopened so a stale result never survives to a different mailbox.
const formTestResult = ref<MailboxTestResult>()
const formTesting = ref(false)
// Per-row "test now" loading state, keyed by mailbox id.
const rowTesting = ref<Record<string, boolean>>({})

const tlsOptions = computed(() =>
  (['none', 'starttls', 'tls'] as const).map((mode) => ({ value: mode, label: mode })),
)

const kindOptions = computed(() =>
  (['imap', 'webhook'] as const).map((kind) => ({ value: kind, label: t(`mailbox.kind.${kind}`) })),
)

const isWebhookKind = computed(() => draft.value.kind === 'webhook')

// Mirrors the backend rule: a webhook-kind mailbox has no credentials to
// dial, so the form's own test only needs an address to send it against
// (internal/api/mailboxtest.go's webhookMailboxTestOut). An imap-kind
// mailbox still needs all three, whether this is a brand new mailbox or a
// password being tried before it is saved.
const canTestForm = computed(() =>
  isWebhookKind.value
    ? Boolean(draft.value.address)
    : Boolean(draft.value.host) && Boolean(draft.value.username) && Boolean(draft.value.password),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('common.name'), width: 'minmax(140px, 1fr)' },
  { key: 'kind', label: t('mailbox.kind.label'), width: '90px', secondary: true },
  { key: 'address', label: t('mailbox.address'), width: 'minmax(180px, 2fr)', mono: true },
  { key: 'endpoint', label: t('mailbox.host'), width: 'minmax(150px, 1fr)', mono: true },
  { key: 'authserv', label: t('mailbox.authservId'), width: '150px', mono: true, secondary: true },
  { key: 'enabled', label: t('mailbox.enabled'), width: '90px' },
  { key: 'health', label: t('mailbox.health'), width: 'minmax(200px, 1fr)' },
  { key: 'actions', label: t('common.actions'), width: '220px', align: 'end' },
])

function startCreate() {
  editing.value = {} as ProbeMailbox
  draft.value = {
    name: '',
    address: '',
    kind: 'imap',
    host: '',
    port: 993,
    tls: 'tls',
    username: '',
    password: '',
    inbox_folder: 'INBOX',
    spam_folder: '',
    authserv_id: '',
    enabled: true,
  }
  formTestResult.value = undefined
}

function startEdit(mailbox: ProbeMailbox) {
  editing.value = mailbox
  draft.value = {
    name: mailbox.name,
    address: mailbox.address,
    kind: mailbox.kind ?? 'imap',
    host: mailbox.host,
    port: mailbox.port,
    tls: mailbox.tls ?? 'tls',
    username: mailbox.username ?? '',
    password: '',
    inbox_folder: mailbox.inbox_folder ?? 'INBOX',
    spam_folder: mailbox.spam_folder ?? '',
    authserv_id: mailbox.authserv_id ?? '',
    enabled: mailbox.enabled ?? true,
  }
  formTestResult.value = undefined
}

/**
 * The form's own "Test" button. A mailbox being edited already has a stored
 * password, so a typed password here is a *candidate* one: it is checked
 * through `{id}/test` with `password` in the body, which never persists and
 * never touches `health` (that would mark a healthy mailbox broken over a
 * typo). A brand new mailbox has no id yet, so the full draft is checked
 * through the credentials-only endpoint instead.
 */
async function testForm() {
  if (!canTestForm.value) return
  formTesting.value = true
  try {
    formTestResult.value = editing.value?.id
      ? await client.post('/api/v1/probe-mailboxes/{mailboxId}/test', {
          params: { path: { mailboxId: editing.value.id } },
          body: { password: draft.value.password },
        })
      : await client.post('/api/v1/probe-mailboxes/test', { body: body() })
  } catch (error) {
    toast.fail(error)
  } finally {
    formTesting.value = false
  }
}

/**
 * A list row's "test now" action: always the stored credentials, no body.
 * Unlike the form's test, this one is recorded as the mailbox's `health`, so
 * the row is refetched afterwards to pick up the new badge.
 */
async function testStored(mailbox: ProbeMailbox) {
  if (!mailbox.id) return
  rowTesting.value = { ...rowTesting.value, [mailbox.id]: true }
  try {
    const result = await client.post('/api/v1/probe-mailboxes/{mailboxId}/test', {
      params: { path: { mailboxId: mailbox.id } },
    })
    if (result.ok) {
      toast.success(`${t('mailboxTest.ok')} (${result.latency_ms}ms)`)
    } else {
      toast.fail(new Error(`${t('mailboxTest.failed')}: ${result.stage}`))
    }
    await list.reload()
  } catch (error) {
    toast.fail(error)
  } finally {
    rowTesting.value = { ...rowTesting.value, [mailbox.id]: false }
  }
}

function body() {
  const base = {
    name: draft.value.name,
    address: draft.value.address,
    kind: draft.value.kind,
    enabled: draft.value.enabled,
    ...(draft.value.authserv_id ? { authserv_id: draft.value.authserv_id } : {}),
  }
  // A webhook-kind mailbox has no host, port, TLS mode, credentials or
  // folders — the API rejects the whole IMAP block with a 422 if it sees any
  // of them, even empty strings, so they must be left out rather than sent
  // blank (internal/api/sending.go's webhookExtraField).
  if (isWebhookKind.value) return base
  return {
    ...base,
    host: draft.value.host,
    port: Number(draft.value.port),
    tls: draft.value.tls,
    inbox_folder: draft.value.inbox_folder || 'INBOX',
    ...(draft.value.username ? { username: draft.value.username } : {}),
    ...(draft.value.password ? { password: draft.value.password } : {}),
    ...(draft.value.spam_folder ? { spam_folder: draft.value.spam_folder } : {}),
  }
}

async function save() {
  saving.value = true
  try {
    const target = editing.value
    if (target?.id && target.version !== undefined) {
      await client.put('/api/v1/probe-mailboxes/{mailboxId}', {
        params: { path: { mailboxId: target.id } },
        body: { ...body(), version: target.version },
      })
    } else {
      await client.post('/api/v1/probe-mailboxes', { body: body() })
    }
    editing.value = null
    list.reset()
    toast.success(t('common.saved'))
  } catch (error) {
    toast.fail(error)
  } finally {
    saving.value = false
  }
}

async function remove(mailbox: ProbeMailbox) {
  if (!mailbox.id) return
  if (!(await confirm({ message: `${t('common.delete')} ${mailbox.name}?`, danger: true }))) return
  try {
    await client.del('/api/v1/probe-mailboxes/{mailboxId}', {
      params: { path: { mailboxId: mailbox.id } },
    })
    list.reset()
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('mailbox.title')" :subtitle="t('mailbox.hint')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" @click="startCreate">{{ t('mailbox.new') }}</SpButton>
      </template>
    </SpPageHeader>

    <SpCard
      v-if="editing"
      :title="editing.id ? editing.name : t('mailbox.new')"
      class="sp-page__block"
    >
      <form class="sp-form-grid" @submit.prevent="save">
        <SpField v-slot="{ id }" :label="t('mailbox.kind.label')">
          <SpSelect :id="id" v-model="draft.kind" :options="kindOptions" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('mailbox.address')" required>
          <SpInput :id="id" v-model="draft.address" type="email" />
        </SpField>
        <p v-if="isWebhookKind" class="sp-form-grid__note">
          {{ t('mailbox.webhookHint') }}
        </p>
        <SpField v-if="!isWebhookKind" v-slot="{ id }" :label="t('mailbox.host')" required>
          <SpInput :id="id" v-model="draft.host" />
        </SpField>
        <SpField v-if="!isWebhookKind" v-slot="{ id }" :label="t('mailbox.port')" required>
          <SpInput :id="id" v-model="draft.port" type="number" :min="1" :max="65535" />
        </SpField>
        <SpField v-if="!isWebhookKind" v-slot="{ id }" :label="t('mailbox.tls')">
          <SpSelect :id="id" v-model="draft.tls" :options="tlsOptions" />
        </SpField>
        <SpField v-if="!isWebhookKind" v-slot="{ id }" :label="t('mailbox.username')">
          <SpInput :id="id" v-model="draft.username" autocomplete="off" />
        </SpField>
        <SpField
          v-if="!isWebhookKind"
          v-slot="{ id, describedBy }"
          :label="t('mailbox.password')"
          :hint="editing.has_password ? t('mailbox.passwordSet') : undefined"
        >
          <SpInput
            :id="id"
            v-model="draft.password"
            type="password"
            autocomplete="new-password"
            :described-by="describedBy"
          />
        </SpField>
        <SpField v-if="!isWebhookKind" v-slot="{ id }" :label="t('mailbox.inboxFolder')">
          <SpInput :id="id" v-model="draft.inbox_folder" />
        </SpField>
        <SpField v-if="!isWebhookKind" v-slot="{ id }" :label="t('mailbox.spamFolder')">
          <SpInput :id="id" v-model="draft.spam_folder" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('mailbox.authservId')"
          :hint="t('mailbox.authservHint')"
        >
          <SpInput :id="id" v-model="draft.authserv_id" :described-by="describedBy" />
        </SpField>
        <SpCheckbox v-model="draft.enabled" :label="t('mailbox.enabled')" />
        <p v-if="editing.id && draft.password" class="sp-form-grid__note">
          {{ t('mailboxTest.unsavedPasswordHint') }}
        </p>
        <div class="sp-form-grid__actions">
          <SpButton @click="editing = null">{{ t('common.cancel') }}</SpButton>
          <SpButton
            :loading="formTesting"
            :disabled="!canTestForm || formTesting"
            @click="testForm"
          >
            {{ t('mailboxTest.run') }}
          </SpButton>
          <SpButton
            type="submit"
            variant="primary"
            :loading="saving"
            :disabled="!draft.name || !draft.address || (!isWebhookKind && !draft.host)"
          >
            {{ t('common.save') }}
          </SpButton>
        </div>
        <MailboxTestPanel :result="formTestResult" />
      </form>
    </SpCard>

    <SpErrorNotice
      :error="list.error.value"
      :on-retry="() => list.reload()"
      class="sp-page__block"
    />

    <SpTable
      :columns="columns"
      :rows="list.items.value"
      :row-key="(row: ProbeMailbox) => row.id ?? row.address"
      :loading="list.loading.value"
      :caption="t('mailbox.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-kind`]="{ row }">
        {{ t(`mailbox.kind.${(row as ProbeMailbox).kind ?? 'imap'}`) }}
      </template>
      <template #[`cell-name`]="{ row }">
        {{ (row as ProbeMailbox).name }}
        <SpSharedBadge v-if="(row as ProbeMailbox).shared" class="sp-row__badge" />
      </template>
      <template #[`cell-endpoint`]="{ row }">
        <span v-if="(row as ProbeMailbox).kind === 'webhook'">—</span>
        <span v-else>{{ (row as ProbeMailbox).host }}:{{ (row as ProbeMailbox).port }}</span>
      </template>
      <template #[`cell-authserv`]="{ row }">{{
        (row as ProbeMailbox).authserv_id || '—'
      }}</template>
      <template #[`cell-enabled`]="{ row }">
        {{ (row as ProbeMailbox).enabled === false ? '✗' : '✓' }}
      </template>
      <template #[`cell-health`]="{ row }">
        <MailboxHealthBadge :health="(row as ProbeMailbox).health" />
        <div class="sp-mailbox-health-times">
          <span>
            {{ t('mailboxTest.healthCheckedAt') }}:
            {{ formatDateTime((row as ProbeMailbox).health?.checked_at, locale) }}
          </span>
          <span>
            {{ t('mailboxTest.healthOkAt') }}:
            {{ formatDateTime((row as ProbeMailbox).health?.last_ok_at, locale) }}
          </span>
        </div>
      </template>
      <template #[`cell-actions`]="{ row }">
        <div class="sp-actions-row">
          <SpButton
            size="sm"
            variant="ghost"
            :loading="rowTesting[(row as ProbeMailbox).id ?? '']"
            @click="testStored(row as ProbeMailbox)"
          >
            {{ t('mailboxTest.retest') }}
          </SpButton>
          <!--
            A shared mailbox can still be tested: reachability is runtime state
            on the `_system` shadow row, not configuration. Editing it is not.
          -->
          <template v-if="!(row as ProbeMailbox).shared">
            <SpButton size="sm" variant="ghost" @click="startEdit(row as ProbeMailbox)">
              {{ t('common.edit') }}
            </SpButton>
            <SpButton size="sm" variant="ghost" @click="remove(row as ProbeMailbox)">
              {{ t('common.delete') }}
            </SpButton>
          </template>
        </div>
      </template>
    </SpTable>
  </div>
</template>

<style scoped>
.sp-form-grid__note {
  grid-column: 1 / -1;
  margin: 0;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}

.sp-row__badge {
  margin-left: var(--sp-space-1);
}

.sp-mailbox-health-times {
  display: flex;
  flex-direction: column;
  margin-top: var(--sp-space-1);
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}
</style>
