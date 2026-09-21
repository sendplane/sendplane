<script setup lang="ts">
import type { components, TLSMode } from '@sendplane/api'
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
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useConfirm } from '../composables/useConfirm.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime } from '../lib/format.js'

type BounceMailbox = components['schemas']['BounceMailbox']
type MailboxProtocol = components['schemas']['MailboxProtocol']
type MailboxTestResult = components['schemas']['MailboxTestResult']

const { client, t, locale } = useSendplane()
const toast = useToast()
const confirm = useConfirm()

const list = useCursorList<BounceMailbox>((params, signal) =>
  client.get('/api/v1/bounce-mailboxes', { params: { query: params }, signal }),
)

const editing = ref<BounceMailbox | null>(null)
const saving = ref(false)
const draft = ref({
  name: '',
  address: '',
  protocol: 'imap' as MailboxProtocol,
  host: '',
  port: 993,
  tls: 'tls' as TLSMode,
  username: '',
  password: '',
  folder: 'INBOX',
  after_process: '',
  enabled: true,
})

const formTestResult = ref<MailboxTestResult>()
const formTesting = ref(false)
const rowTesting = ref<Record<string, boolean>>({})

const tlsOptions = computed(() =>
  (['none', 'starttls', 'tls'] as const).map((mode) => ({ value: mode, label: mode })),
)
const protocolOptions = computed(() =>
  (['imap', 'pop3'] as const).map((protocol) => ({ value: protocol, label: protocol })),
)

const canTestForm = computed(
  () => Boolean(draft.value.host) && Boolean(draft.value.username) && Boolean(draft.value.password),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('common.name'), width: 'minmax(140px, 1fr)' },
  { key: 'endpoint', label: t('bounceMailbox.host'), width: 'minmax(150px, 1fr)', mono: true },
  { key: 'protocol', label: t('bounceMailbox.protocol'), width: '90px', secondary: true },
  { key: 'enabled', label: t('bounceMailbox.enabled'), width: '90px' },
  { key: 'health', label: t('bounceMailbox.health'), width: 'minmax(200px, 1fr)' },
  { key: 'actions', label: t('common.actions'), width: '220px', align: 'end' },
])

function resetDraft() {
  draft.value = {
    name: '',
    address: '',
    protocol: 'imap',
    host: '',
    port: 993,
    tls: 'tls',
    username: '',
    password: '',
    folder: 'INBOX',
    after_process: '',
    enabled: true,
  }
}

function startCreate() {
  editing.value = {} as BounceMailbox
  resetDraft()
  formTestResult.value = undefined
}

function startEdit(mailbox: BounceMailbox) {
  editing.value = mailbox
  draft.value = {
    name: mailbox.name,
    address: mailbox.address ?? '',
    protocol: mailbox.protocol ?? 'imap',
    host: mailbox.host,
    port: mailbox.port,
    tls: mailbox.tls ?? 'tls',
    username: mailbox.username ?? '',
    password: '',
    folder: mailbox.folder ?? 'INBOX',
    after_process: mailbox.after_process ?? '',
    enabled: mailbox.enabled ?? true,
  }
  formTestResult.value = undefined
}

function body() {
  return {
    name: draft.value.name,
    protocol: draft.value.protocol,
    host: draft.value.host,
    port: Number(draft.value.port),
    tls: draft.value.tls,
    enabled: draft.value.enabled,
    folder: draft.value.folder || 'INBOX',
    ...(draft.value.address ? { address: draft.value.address } : {}),
    ...(draft.value.username ? { username: draft.value.username } : {}),
    ...(draft.value.password ? { password: draft.value.password } : {}),
    ...(draft.value.after_process ? { after_process: draft.value.after_process } : {}),
  }
}

async function save() {
  saving.value = true
  try {
    const target = editing.value
    if (target?.id && target.version !== undefined) {
      await client.put('/api/v1/bounce-mailboxes/{mailboxId}', {
        params: { path: { mailboxId: target.id } },
        body: { ...body(), version: target.version },
      })
    } else {
      await client.post('/api/v1/bounce-mailboxes', { body: body() })
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

async function remove(mailbox: BounceMailbox) {
  if (!mailbox.id) return
  if (!(await confirm({ message: `${t('common.delete')} ${mailbox.name}?`, danger: true }))) return
  try {
    await client.del('/api/v1/bounce-mailboxes/{mailboxId}', {
      params: { path: { mailboxId: mailbox.id } },
    })
    list.reset()
  } catch (error) {
    toast.fail(error)
  }
}

async function testForm() {
  if (!canTestForm.value) return
  formTesting.value = true
  try {
    formTestResult.value = editing.value?.id
      ? await client.post('/api/v1/bounce-mailboxes/{mailboxId}/test', {
          params: { path: { mailboxId: editing.value.id } },
          body: { password: draft.value.password },
        })
      : await client.post('/api/v1/bounce-mailboxes/test', { body: body() })
  } catch (error) {
    toast.fail(error)
  } finally {
    formTesting.value = false
  }
}

async function testStored(mailbox: BounceMailbox) {
  if (!mailbox.id) return
  rowTesting.value = { ...rowTesting.value, [mailbox.id]: true }
  try {
    const result = await client.post('/api/v1/bounce-mailboxes/{mailboxId}/test', {
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
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('bounceMailbox.title')" :subtitle="t('bounceMailbox.hint')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" @click="startCreate">{{ t('bounceMailbox.new') }}</SpButton>
      </template>
    </SpPageHeader>

    <SpCard
      v-if="editing"
      :title="editing.id ? editing.name : t('bounceMailbox.new')"
      class="sp-page__block"
    >
      <form class="sp-form-grid" @submit.prevent="save">
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('bounceMailbox.address')">
          <SpInput :id="id" v-model="draft.address" type="email" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('bounceMailbox.protocol')">
          <SpSelect :id="id" v-model="draft.protocol" :options="protocolOptions" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('bounceMailbox.host')" required>
          <SpInput :id="id" v-model="draft.host" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('bounceMailbox.port')" required>
          <SpInput :id="id" v-model="draft.port" type="number" :min="1" :max="65535" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('bounceMailbox.tls')">
          <SpSelect :id="id" v-model="draft.tls" :options="tlsOptions" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('bounceMailbox.username')">
          <SpInput :id="id" v-model="draft.username" autocomplete="off" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('bounceMailbox.password')"
          :hint="editing.has_password ? t('bounceMailbox.passwordSet') : undefined"
        >
          <SpInput
            :id="id"
            v-model="draft.password"
            type="password"
            autocomplete="new-password"
            :described-by="describedBy"
          />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('bounceMailbox.folder')"
          :hint="t('bounceMailbox.folderHint')"
        >
          <SpInput :id="id" v-model="draft.folder" :described-by="describedBy" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('bounceMailbox.afterProcess')"
          :hint="t('bounceMailbox.afterProcessHint')"
        >
          <SpInput :id="id" v-model="draft.after_process" :described-by="describedBy" />
        </SpField>
        <SpCheckbox v-model="draft.enabled" :label="t('bounceMailbox.enabled')" />
        <p v-if="editing.id && draft.password" class="sp-form-grid__note">
          {{ t('mailboxTest.unsavedPasswordHint') }}
        </p>
        <div class="sp-form-grid__actions">
          <SpButton @click="editing = null">{{ t('common.cancel') }}</SpButton>
          <SpButton :loading="formTesting" :disabled="!canTestForm || formTesting" @click="testForm">
            {{ t('mailboxTest.run') }}
          </SpButton>
          <SpButton
            type="submit"
            variant="primary"
            :loading="saving"
            :disabled="!draft.name || !draft.host"
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
      :row-key="(row: BounceMailbox) => row.id ?? row.name"
      :loading="list.loading.value"
      :caption="t('bounceMailbox.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-endpoint`]="{ row }">
        {{ (row as BounceMailbox).host }}:{{ (row as BounceMailbox).port }}
      </template>
      <template #[`cell-protocol`]="{ row }">
        {{ (row as BounceMailbox).protocol ?? 'imap' }}
      </template>
      <template #[`cell-enabled`]="{ row }">
        {{ (row as BounceMailbox).enabled === false ? '✗' : '✓' }}
      </template>
      <template #[`cell-health`]="{ row }">
        <MailboxHealthBadge :health="(row as BounceMailbox).health" />
        <div class="sp-mailbox-health-times">
          <span>
            {{ t('mailboxTest.healthCheckedAt') }}:
            {{ formatDateTime((row as BounceMailbox).health?.checked_at, locale) }}
          </span>
          <span>
            {{ t('mailboxTest.healthOkAt') }}:
            {{ formatDateTime((row as BounceMailbox).health?.last_ok_at, locale) }}
          </span>
        </div>
      </template>
      <template #[`cell-actions`]="{ row }">
        <div class="sp-actions-row">
          <SpButton
            size="sm"
            variant="ghost"
            :loading="rowTesting[(row as BounceMailbox).id ?? '']"
            @click="testStored(row as BounceMailbox)"
          >
            {{ t('mailboxTest.retest') }}
          </SpButton>
          <SpButton size="sm" variant="ghost" @click="startEdit(row as BounceMailbox)">
            {{ t('common.edit') }}
          </SpButton>
          <SpButton size="sm" variant="ghost" @click="remove(row as BounceMailbox)">
            {{ t('common.delete') }}
          </SpButton>
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

.sp-mailbox-health-times {
  display: flex;
  flex-direction: column;
  margin-top: var(--sp-space-1);
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}
</style>
