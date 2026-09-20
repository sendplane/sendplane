<script setup lang="ts">
import type { ProbeMailbox, TLSMode } from '@sendplane/api'
import { computed, ref } from 'vue'

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

const { client, t } = useSendplane()
const toast = useToast()
const confirm = useConfirm()

const list = useCursorList<ProbeMailbox>((params, signal) =>
  client.get('/api/v1/probe-mailboxes', { params: { query: params }, signal }),
)

const editing = ref<ProbeMailbox | null>(null)
const saving = ref(false)
const draft = ref({
  name: '',
  address: '',
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

const tlsOptions = computed(() =>
  (['none', 'starttls', 'tls'] as const).map((mode) => ({ value: mode, label: mode })),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('common.name'), width: 'minmax(140px, 1fr)' },
  { key: 'address', label: t('mailbox.address'), width: 'minmax(180px, 2fr)', mono: true },
  { key: 'endpoint', label: t('mailbox.host'), width: 'minmax(150px, 1fr)', mono: true },
  { key: 'authserv', label: t('mailbox.authservId'), width: '150px', mono: true, secondary: true },
  { key: 'enabled', label: t('mailbox.enabled'), width: '90px' },
  { key: 'actions', label: t('common.actions'), width: '150px', align: 'end' },
])

function startCreate() {
  editing.value = {} as ProbeMailbox
  draft.value = {
    name: '',
    address: '',
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
}

function startEdit(mailbox: ProbeMailbox) {
  editing.value = mailbox
  draft.value = {
    name: mailbox.name,
    address: mailbox.address,
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
}

function body() {
  return {
    name: draft.value.name,
    address: draft.value.address,
    host: draft.value.host,
    port: Number(draft.value.port),
    tls: draft.value.tls,
    enabled: draft.value.enabled,
    inbox_folder: draft.value.inbox_folder || 'INBOX',
    ...(draft.value.username ? { username: draft.value.username } : {}),
    ...(draft.value.password ? { password: draft.value.password } : {}),
    ...(draft.value.spam_folder ? { spam_folder: draft.value.spam_folder } : {}),
    ...(draft.value.authserv_id ? { authserv_id: draft.value.authserv_id } : {}),
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
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('mailbox.address')" required>
          <SpInput :id="id" v-model="draft.address" type="email" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('mailbox.host')" required>
          <SpInput :id="id" v-model="draft.host" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('mailbox.port')" required>
          <SpInput :id="id" v-model="draft.port" type="number" :min="1" :max="65535" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('mailbox.tls')">
          <SpSelect :id="id" v-model="draft.tls" :options="tlsOptions" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('mailbox.username')">
          <SpInput :id="id" v-model="draft.username" autocomplete="off" />
        </SpField>
        <SpField
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
        <SpField v-slot="{ id }" :label="t('mailbox.inboxFolder')">
          <SpInput :id="id" v-model="draft.inbox_folder" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('mailbox.spamFolder')">
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
        <div class="sp-form-grid__actions">
          <SpButton @click="editing = null">{{ t('common.cancel') }}</SpButton>
          <SpButton
            type="submit"
            variant="primary"
            :loading="saving"
            :disabled="!draft.name || !draft.address || !draft.host"
          >
            {{ t('common.save') }}
          </SpButton>
        </div>
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
      <template #[`cell-endpoint`]="{ row }">
        {{ (row as ProbeMailbox).host }}:{{ (row as ProbeMailbox).port }}
      </template>
      <template #[`cell-authserv`]="{ row }">{{
        (row as ProbeMailbox).authserv_id || '—'
      }}</template>
      <template #[`cell-enabled`]="{ row }">
        {{ (row as ProbeMailbox).enabled === false ? '✗' : '✓' }}
      </template>
      <template #[`cell-actions`]="{ row }">
        <div class="sp-actions-row">
          <SpButton size="sm" variant="ghost" @click="startEdit(row as ProbeMailbox)">
            {{ t('common.edit') }}
          </SpButton>
          <SpButton size="sm" variant="ghost" @click="remove(row as ProbeMailbox)">
            {{ t('common.delete') }}
          </SpButton>
        </div>
      </template>
    </SpTable>
  </div>
</template>
