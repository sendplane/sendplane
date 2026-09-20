<script setup lang="ts">
import type { Template } from '@sendplane/api'
import { computed } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpLink from '../components/SpLink.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useConfirm } from '../composables/useConfirm.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime, shortId } from '../lib/format.js'

const { client, t, locale, navigate } = useSendplane()
const toast = useToast()
const confirm = useConfirm()

const list = useCursorList<Template>((params, signal) =>
  client.get('/api/v1/templates', { params: { query: params }, signal }),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('common.name'), width: 'minmax(180px, 2fr)' },
  { key: 'subject', label: t('template.subject'), width: 'minmax(180px, 2fr)', secondary: true },
  { key: 'mode', label: t('template.mode'), width: '100px' },
  {
    key: 'published',
    label: t('template.publishedVersion'),
    width: '160px',
    mono: true,
    secondary: true,
  },
  { key: 'updated', label: t('common.updated'), width: '180px', secondary: true },
  { key: 'actions', label: t('common.actions'), width: '90px', align: 'end' },
])

async function remove(template: Template) {
  if (!template.id) return
  const ok = await confirm({ message: `${t('common.delete')} ${template.name}?`, danger: true })
  if (!ok) return
  try {
    await client.del('/api/v1/templates/{templateId}', {
      params: { path: { templateId: template.id } },
    })
    list.reset()
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('template.title')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" @click="navigate({ name: 'template.new' })">
          {{ t('template.new') }}
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
      :row-key="(row: Template) => row.id ?? row.name"
      :loading="list.loading.value"
      :caption="t('template.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-name`]="{ row }">
        <SpLink :to="{ name: 'template', params: { templateId: (row as Template).id! } }">
          {{ (row as Template).name }}
        </SpLink>
      </template>
      <template #[`cell-mode`]="{ row }">
        {{ t(`template.modes.${(row as Template).mode}`) }}
      </template>
      <template #[`cell-published`]="{ row }">
        {{ shortId((row as Template).published_version_id) }}
      </template>
      <template #[`cell-updated`]="{ row }">
        {{ formatDateTime((row as Template).updated_at, locale) }}
      </template>
      <template #[`cell-actions`]="{ row }">
        <SpButton size="sm" variant="ghost" @click="remove(row as Template)">
          {{ t('common.delete') }}
        </SpButton>
      </template>
    </SpTable>
  </div>
</template>
