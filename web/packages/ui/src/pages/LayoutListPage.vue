<script setup lang="ts">
import type { Layout } from '@sendplane/api'
import { computed } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpLink from '../components/SpLink.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useApiToast } from '../composables/useApiToast.js'
import { useConfirm } from '../composables/useConfirm.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useSendplane } from '../context.js'
import { formatDateTime } from '../lib/format.js'

const { client, t, locale, navigate, systemTenant } = useSendplane()
const toast = useApiToast()
const confirm = useConfirm()

const list = useCursorList<Layout>((params, signal) =>
  client.get('/api/v1/layouts', { params: { query: params }, signal }),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('common.name'), width: 'minmax(200px, 3fr)' },
  { key: 'mode', label: t('layout.mode'), width: '100px' },
  { key: 'updated', label: t('common.updated'), width: '200px', secondary: true },
  { key: 'actions', label: t('common.actions'), width: '90px', align: 'end' },
])

async function remove(layout: Layout) {
  if (!layout.id) return
  const ok = await confirm({ message: `${t('common.delete')} ${layout.name}?`, danger: true })
  if (!ok) return
  try {
    await client.del('/api/v1/layouts/{layoutId}', { params: { path: { layoutId: layout.id } } })
    list.reset()
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('layout.title')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <!-- Content belongs to a tenant; the system tenant only reads it. -->
        <SpButton v-if="!systemTenant" variant="primary" @click="navigate({ name: 'layout.new' })">
          {{ t('layout.new') }}
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
      :row-key="(row: Layout) => row.id ?? row.name"
      :loading="list.loading.value"
      :caption="t('layout.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-name`]="{ row }">
        <SpLink :to="{ name: 'layout', params: { layoutId: (row as Layout).id! } }">
          {{ (row as Layout).name }}
        </SpLink>
      </template>
      <template #[`cell-mode`]="{ row }">{{
        t(`template.modes.${(row as Layout).mode}`)
      }}</template>
      <template #[`cell-updated`]="{ row }">
        {{ formatDateTime((row as Layout).updated_at, locale) }}
      </template>
      <template #[`cell-actions`]="{ row }">
        <SpButton v-if="!systemTenant" size="sm" variant="ghost" @click="remove(row as Layout)">
          {{ t('common.delete') }}
        </SpButton>
        <span v-else>—</span>
      </template>
    </SpTable>
  </div>
</template>
