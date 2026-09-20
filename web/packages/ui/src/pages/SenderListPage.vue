<script setup lang="ts">
import type { Sender } from '@sendplane/api'
import { computed } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpLink from '../components/SpLink.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useConfirm } from '../composables/useConfirm.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime } from '../lib/format.js'

const { client, t, locale, navigate } = useSendplane()
const toast = useToast()
const confirm = useConfirm()

const list = useCursorList<Sender>((params, signal) =>
  client.get('/api/v1/senders', { params: { query: params }, signal }),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('common.name'), width: 'minmax(150px, 2fr)' },
  { key: 'from', label: t('sender.fromEmail'), width: 'minmax(180px, 2fr)', mono: true },
  { key: 'health', label: t('sender.health'), width: '120px' },
  { key: 'reason', label: t('sender.healthReason'), width: 'minmax(140px, 2fr)', secondary: true },
  { key: 'checked', label: t('sender.checkedAt'), width: '190px', secondary: true },
  { key: 'actions', label: t('common.actions'), width: '90px', align: 'end' },
])

async function remove(sender: Sender) {
  if (!sender.id) return
  const ok = await confirm({ message: `${t('common.delete')} ${sender.name}?`, danger: true })
  if (!ok) return
  try {
    await client.del('/api/v1/senders/{senderId}', { params: { path: { senderId: sender.id } } })
    list.reset()
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('sender.title')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" @click="navigate({ name: 'sender.new' })">
          {{ t('sender.new') }}
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
      :row-key="(row: Sender) => row.id ?? row.name"
      :loading="list.loading.value"
      :caption="t('sender.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-name`]="{ row }">
        <SpLink :to="{ name: 'sender', params: { senderId: (row as Sender).id! } }">
          {{ (row as Sender).name }}
        </SpLink>
      </template>
      <template #[`cell-from`]="{ row }">{{ (row as Sender).from_email }}</template>
      <template #[`cell-health`]="{ row }">
        <SpStatusBadge
          kind="health"
          :value="(row as Sender).health"
          :title="(row as Sender).health_reason"
        />
      </template>
      <template #[`cell-reason`]="{ row }">{{ (row as Sender).health_reason || '—' }}</template>
      <template #[`cell-checked`]="{ row }">
        {{ formatDateTime((row as Sender).health_checked_at, locale) }}
      </template>
      <template #[`cell-actions`]="{ row }">
        <SpButton size="sm" variant="ghost" @click="remove(row as Sender)">
          {{ t('common.delete') }}
        </SpButton>
      </template>
    </SpTable>
  </div>
</template>
