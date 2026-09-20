<script setup lang="ts">
import type { ProbeRun, Sender } from '@sendplane/api'
import { computed, ref, watch } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpJsonView from '../components/SpJsonView.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useAsync } from '../composables/useAsync.js'
import { useConfirm } from '../composables/useConfirm.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime, shortId } from '../lib/format.js'

const props = defineProps<{ senderId?: string }>()

const { client, t, locale, navigate } = useSendplane()
const toast = useToast()
const confirm = useConfirm()

const draft = ref({
  name: '',
  from_name: '',
  from_email: '',
  reply_to: '',
  transport_id: '',
  domain_id: '',
})
const current = ref<Sender | undefined>()
const saving = ref(false)
const probing = ref(false)

const loaded = useAsync(
  (signal) =>
    props.senderId
      ? client.get('/api/v1/senders/{senderId}', {
          params: { path: { senderId: props.senderId } },
          signal,
        })
      : Promise.resolve(undefined as Sender | undefined),
  { watch: () => props.senderId },
)

watch(loaded.data, (sender) => {
  if (!sender) return
  current.value = sender
  draft.value = {
    name: sender.name,
    from_name: sender.from_name ?? '',
    from_email: sender.from_email,
    reply_to: sender.reply_to ?? '',
    transport_id: sender.transport_id ?? '',
    domain_id: sender.domain_id ?? '',
  }
})

const transports = useAsync((signal) =>
  client.get('/api/v1/transports', { params: { query: { limit: 200 } }, signal }),
)
const domains = useAsync((signal) =>
  client.get('/api/v1/sending-domains', { params: { query: { limit: 200 } }, signal }),
)

/**
 * The worst-of verdict across probe mailboxes, plus the per-mailbox detail
 * (architecture 11.4). It is the only place a sender's real deliverability
 * shows up, so it is loaded even on the edit screen.
 */
const health = useAsync(
  (signal) =>
    props.senderId
      ? client.get('/api/v1/senders/{senderId}/health', {
          params: { path: { senderId: props.senderId } },
          signal,
        })
      : Promise.resolve(undefined),
  { watch: () => props.senderId },
)

const runs = useCursorList<ProbeRun>(
  (params, signal) =>
    props.senderId
      ? client.get('/api/v1/probe-runs', {
          params: { query: { ...params, sender_id: props.senderId } },
          signal,
        })
      : Promise.resolve({ items: [] as ProbeRun[] }),
  { limit: 20, watch: () => props.senderId },
)

const transportOptions = computed(() =>
  (transports.data.value?.items ?? []).map((transport) => ({
    value: transport.id ?? '',
    label: `${transport.name} (${transport.host}:${transport.port})`,
  })),
)

const domainOptions = computed(() =>
  (domains.data.value?.items ?? []).map((domain) => ({
    value: domain.id ?? '',
    label: domain.domain,
  })),
)

const verdictHint = computed(() => {
  switch (health.data.value?.status ?? current.value?.health) {
    case 'green':
      return t('sender.verdictGreen')
    case 'yellow':
      return t('sender.verdictYellow')
    case 'red':
      return t('sender.verdictRed')
    default:
      return t('sender.verdictUnknown')
  }
})

const runColumns = computed<TableColumn[]>(() => [
  { key: 'created', label: t('common.created'), width: '180px' },
  { key: 'status', label: t('sender.verdict'), width: '110px' },
  { key: 'mailbox', label: t('sender.mailbox'), width: '130px', mono: true },
  { key: 'folder', label: t('sender.folder'), width: '90px' },
  { key: 'auth', label: 'SPF / DKIM / DMARC', width: 'minmax(180px, 1fr)', mono: true },
  { key: 'tls', label: t('sender.tls'), width: '70px', secondary: true },
  { key: 'reason', label: t('common.reason'), width: 'minmax(150px, 1fr)', secondary: true },
])

function body() {
  return {
    name: draft.value.name,
    from_email: draft.value.from_email,
    transport_id: draft.value.transport_id,
    ...(draft.value.from_name ? { from_name: draft.value.from_name } : {}),
    ...(draft.value.reply_to ? { reply_to: draft.value.reply_to } : {}),
    ...(draft.value.domain_id ? { domain_id: draft.value.domain_id } : {}),
  }
}

async function save() {
  saving.value = true
  try {
    if (props.senderId && current.value?.version !== undefined) {
      await client.put('/api/v1/senders/{senderId}', {
        params: { path: { senderId: props.senderId } },
        body: { ...body(), version: current.value.version },
      })
      await loaded.reload()
    } else {
      const created = await client.post('/api/v1/senders', { body: body() })
      if (created.id) navigate({ name: 'sender', params: { senderId: created.id } })
    }
    toast.success(t('common.saved'))
  } catch (error) {
    toast.fail(error)
  } finally {
    saving.value = false
  }
}

/**
 * Replaces the usual "test SMTP connection" button: a probe goes out through
 * the real transport with real DKIM signing, so it also refreshes the transport
 * circuit state (architecture 11.2).
 */
async function probeNow() {
  if (!props.senderId) return
  if (!(await confirm(t('sender.confirmProbe')))) return
  probing.value = true
  try {
    const result = await client.post('/api/v1/senders/{senderId}/probe', {
      params: { path: { senderId: props.senderId } },
      body: {},
    })
    toast.success(t('sender.probeQueued', { count: result.runs.length }))
    runs.reset()
  } catch (error) {
    toast.fail(error)
  } finally {
    probing.value = false
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="senderId ? draft.name || t('sender.one') : t('sender.new')">
      <template #breadcrumb>
        <a class="sp-link" href="#" @click.prevent="navigate({ name: 'senders' })">
          ← {{ t('sender.title') }}
        </a>
      </template>
      <template #badge>
        <SpStatusBadge
          v-if="senderId"
          kind="health"
          :value="health.data.value?.status ?? current?.health"
          :title="health.data.value?.reason ?? current?.health_reason"
        />
      </template>
      <template #actions>
        <SpButton v-if="senderId" :loading="probing" @click="probeNow">
          {{ t('sender.probeNow') }}
        </SpButton>
        <SpButton
          variant="primary"
          :loading="saving"
          :disabled="!draft.name || !draft.from_email || !draft.transport_id"
          @click="save"
        >
          {{ t('common.save') }}
        </SpButton>
      </template>
    </SpPageHeader>

    <SpErrorNotice
      :error="loaded.error.value"
      :on-retry="() => loaded.reload()"
      class="sp-page__block"
    />

    <SpCard class="sp-page__block">
      <form class="sp-form-grid" @submit.prevent="save">
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('sender.fromName')">
          <SpInput :id="id" v-model="draft.from_name" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('sender.fromEmail')" required>
          <SpInput :id="id" v-model="draft.from_email" type="email" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('sender.replyTo')">
          <SpInput :id="id" v-model="draft.reply_to" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('sender.transport')" required>
          <SpSelect
            :id="id"
            v-model="draft.transport_id"
            :options="transportOptions"
            :placeholder="t('common.none')"
          />
        </SpField>
        <SpField v-slot="{ id }" :label="t('sender.domain')">
          <SpSelect
            :id="id"
            v-model="draft.domain_id"
            :options="domainOptions"
            :placeholder="t('common.none')"
          />
        </SpField>
      </form>
    </SpCard>

    <SpCard v-if="senderId" :title="t('sender.health')" class="sp-page__block">
      <template #actions>
        <SpButton size="sm" :loading="health.loading.value" @click="health.reload()">
          {{ t('common.refresh') }}
        </SpButton>
      </template>
      <p class="sp-note sp-page__block">{{ verdictHint }}</p>
      <dl class="sp-detail-list">
        <dt>{{ t('sender.verdict') }}</dt>
        <dd><SpStatusBadge kind="health" :value="health.data.value?.status" /></dd>
        <dt>{{ t('sender.healthReason') }}</dt>
        <dd>{{ health.data.value?.reason || '—' }}</dd>
        <dt>{{ t('sender.transportStatus') }}</dt>
        <dd><SpStatusBadge kind="transport" :value="health.data.value?.transport_status" /></dd>
        <dt>{{ t('sender.domainStatus') }}</dt>
        <dd><SpStatusBadge kind="health" :value="health.data.value?.domain_status" /></dd>
        <dt>{{ t('sender.checkedAt') }}</dt>
        <dd>{{ formatDateTime(health.data.value?.checked_at, locale) }}</dd>
      </dl>
    </SpCard>

    <SpCard v-if="senderId" :title="t('sender.probeHistory')" :padded="false">
      <SpTable
        :columns="runColumns"
        :rows="runs.items.value"
        :row-key="(row: ProbeRun) => row.id"
        :loading="runs.loading.value"
        :caption="t('sender.probeHistory')"
        :has-next="runs.hasNext.value"
        :has-previous="runs.hasPrevious.value"
        :page-number="runs.pageNumber.value"
        :empty-title="t('sender.noRuns')"
        @next="runs.next()"
        @previous="runs.previous()"
      >
        <template #[`cell-created`]="{ row }">
          {{ formatDateTime((row as ProbeRun).created_at ?? (row as ProbeRun).started_at, locale) }}
        </template>
        <template #[`cell-status`]="{ row }">
          <SpStatusBadge kind="health" :value="(row as ProbeRun).status" />
        </template>
        <template #[`cell-mailbox`]="{ row }">{{ shortId((row as ProbeRun).mailbox_id) }}</template>
        <template #[`cell-folder`]="{ row }">{{ (row as ProbeRun).folder || '—' }}</template>
        <template #[`cell-auth`]="{ row }">
          {{ (row as ProbeRun).spf || '—' }} / {{ (row as ProbeRun).dkim || '—' }} /
          {{ (row as ProbeRun).dmarc || '—' }}
        </template>
        <template #[`cell-tls`]="{ row }">
          {{ (row as ProbeRun).tls === undefined ? '—' : (row as ProbeRun).tls ? '✓' : '✗' }}
        </template>
        <template #[`cell-reason`]="{ row }">
          <SpJsonView
            v-if="(row as ProbeRun).dns"
            :value="(row as ProbeRun).dns"
            collapsed
            :label="(row as ProbeRun).reason || t('sender.dnsChecks')"
          />
          <span v-else>{{ (row as ProbeRun).reason || '—' }}</span>
        </template>
      </SpTable>
    </SpCard>
  </div>
</template>
