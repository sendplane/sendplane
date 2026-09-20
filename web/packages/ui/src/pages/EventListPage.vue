<script setup lang="ts">
import type { OutboxEvent, OutboxStatus } from '@sendplane/api'
import { computed, ref } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpJsonView from '../components/SpJsonView.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import SpTabs, { type TabItem } from '../components/SpTabs.vue'
import { useConfirm } from '../composables/useConfirm.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime, shortId } from '../lib/format.js'

const { client, t, locale } = useSendplane()
const toast = useToast()
const confirm = useConfirm()

const tab = ref<'outbox' | 'dead-letter'>('outbox')
const status = ref('')
const typeFilter = ref('')

const tabs = computed<TabItem[]>(() => [
  { value: 'outbox', label: t('event.outbox') },
  { value: 'dead-letter', label: t('event.deadLetter') },
])

const statusOptions = computed(() =>
  (['pending', 'delivered', 'failed'] as const).map((value) => ({
    value,
    label: t(`status.outbox.${value}`),
  })),
)

const types = computed(() =>
  typeFilter.value
    .split(',')
    .map((entry) => entry.trim())
    .filter(Boolean),
)

const list = useCursorList<OutboxEvent>(
  (params, signal) =>
    tab.value === 'dead-letter'
      ? // The dead-letter route is a separate operation in the spec rather than
        // a `status=failed` filter, because replay lives on the same screen.
        client.get('/api/v1/events/dead-letter', { params: { query: params }, signal })
      : client.get('/api/v1/events', {
          params: {
            query: {
              ...params,
              ...(status.value ? { status: status.value as OutboxStatus } : {}),
              ...(types.value.length ? { type: types.value } : {}),
            },
          },
          signal,
        }),
  { watch: [tab, status, types] },
)

const columns = computed<TableColumn[]>(() => [
  { key: 'created', label: t('common.created'), width: '180px' },
  { key: 'type', label: t('common.type'), width: 'minmax(160px, 1fr)', mono: true },
  { key: 'status', label: t('common.status'), width: '120px' },
  { key: 'attempts', label: t('event.attempts'), width: '90px', align: 'end' },
  { key: 'error', label: t('event.lastError'), width: 'minmax(160px, 2fr)', secondary: true },
  { key: 'payload', label: t('event.payload'), width: '150px', secondary: true },
  { key: 'actions', label: t('common.actions'), width: '100px', align: 'end' },
])

async function replay(event: OutboxEvent) {
  if (!(await confirm(t('event.confirmReplay')))) return
  try {
    await client.post('/api/v1/events/{eventId}/replay', {
      params: { path: { eventId: event.id } },
    })
    toast.success(t('event.replayed'))
    list.reload()
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('event.title')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
      </template>
    </SpPageHeader>

    <SpTabs v-model="tab" :tabs="tabs" :aria-label="t('event.title')" class="sp-page__block" />

    <div v-if="tab === 'outbox'" class="sp-toolbar">
      <SpField v-slot="{ id }" :label="t('event.filterStatus')">
        <SpSelect
          :id="id"
          v-model="status"
          :options="statusOptions"
          :placeholder="t('common.none')"
        />
      </SpField>
      <SpField v-slot="{ id }" :label="t('event.filterType')">
        <SpInput :id="id" v-model="typeFilter" placeholder="delivery.failed, campaign.completed" />
      </SpField>
    </div>

    <SpErrorNotice
      :error="list.error.value"
      :on-retry="() => list.reload()"
      class="sp-page__block"
    />

    <SpTable
      :columns="columns"
      :rows="list.items.value"
      :row-key="(row: OutboxEvent) => row.id"
      :loading="list.loading.value"
      :caption="t('event.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-created`]="{ row }">
        {{ formatDateTime((row as OutboxEvent).created_at, locale) }}
      </template>
      <template #[`cell-status`]="{ row }">
        <SpStatusBadge
          kind="outbox"
          :value="(row as OutboxEvent).status"
          :title="(row as OutboxEvent).last_error"
        />
      </template>
      <template #[`cell-error`]="{ row }">{{ (row as OutboxEvent).last_error || '—' }}</template>
      <template #[`cell-payload`]="{ row }">
        <SpJsonView
          :value="(row as OutboxEvent).payload"
          collapsed
          :label="shortId((row as OutboxEvent).id)"
        />
      </template>
      <template #[`cell-actions`]="{ row }">
        <SpButton size="sm" variant="ghost" @click="replay(row as OutboxEvent)">
          {{ t('event.replay') }}
        </SpButton>
      </template>
    </SpTable>
  </div>
</template>
