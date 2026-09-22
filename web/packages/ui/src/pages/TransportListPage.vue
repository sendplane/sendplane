<script setup lang="ts">
import type { Transport } from '@sendplane/api'
import { computed } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpLink from '../components/SpLink.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSharedBadge from '../components/SpSharedBadge.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useApiToast } from '../composables/useApiToast.js'
import { useConfirm } from '../composables/useConfirm.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useSendplane } from '../context.js'
import { formatDateTime, formatNumber } from '../lib/format.js'

const { client, t, locale, navigate } = useSendplane()
const toast = useApiToast()
const confirm = useConfirm()

const list = useCursorList<Transport>((params, signal) =>
  client.get('/api/v1/transports', { params: { query: params }, signal }),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('common.name'), width: 'minmax(160px, 2fr)' },
  { key: 'endpoint', label: t('transport.host'), width: 'minmax(160px, 2fr)', mono: true },
  { key: 'status', label: t('transport.health'), width: '130px' },
  {
    key: 'rate',
    label: t('transport.ratePerSecond'),
    width: '110px',
    align: 'end',
    secondary: true,
  },
  { key: 'changed', label: t('transport.statusChangedAt'), width: '190px', secondary: true },
  { key: 'actions', label: t('common.actions'), width: '90px', align: 'end' },
])

async function remove(transport: Transport) {
  if (!transport.id) return
  const ok = await confirm({ message: `${t('common.delete')} ${transport.name}?`, danger: true })
  if (!ok) return
  try {
    await client.del('/api/v1/transports/{transportId}', {
      params: { path: { transportId: transport.id } },
    })
    list.reset()
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('transport.title')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" @click="navigate({ name: 'transport.new' })">
          {{ t('transport.new') }}
        </SpButton>
      </template>
    </SpPageHeader>

    <SpErrorNotice
      :error="list.error.value"
      :on-retry="() => list.reload()"
      class="sp-page__block"
    />

    <SpTable
      :columns="columns"
      :rows="list.items.value"
      :row-key="(row: Transport) => row.id ?? row.name"
      :loading="list.loading.value"
      :caption="t('transport.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-name`]="{ row }">
        <SpLink :to="{ name: 'transport', params: { transportId: (row as Transport).id! } }">
          {{ (row as Transport).name }}
        </SpLink>
        <SpSharedBadge v-if="(row as Transport).shared" class="sp-row__badge" />
      </template>
      <template #[`cell-endpoint`]="{ row }">
        {{ (row as Transport).host }}:{{ (row as Transport).port }}
      </template>
      <template #[`cell-status`]="{ row }">
        <SpStatusBadge
          kind="transport"
          :value="(row as Transport).status"
          :title="(row as Transport).status_reason"
        />
      </template>
      <template #[`cell-rate`]="{ row }">
        {{ formatNumber((row as Transport).rate_per_second, locale) }}
      </template>
      <template #[`cell-changed`]="{ row }">
        {{ formatDateTime((row as Transport).status_changed_at, locale) }}
      </template>
      <template #[`cell-actions`]="{ row }">
        <!-- Shared rows are the operator's configuration: no delete to offer. -->
        <SpButton
          v-if="!(row as Transport).shared"
          size="sm"
          variant="ghost"
          @click="remove(row as Transport)"
        >
          {{ t('common.delete') }}
        </SpButton>
        <span v-else>—</span>
      </template>
    </SpTable>
  </div>
</template>

<style scoped>
.sp-row__badge {
  margin-left: var(--sp-space-1);
}
</style>
