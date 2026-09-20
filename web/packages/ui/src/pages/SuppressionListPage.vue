<script setup lang="ts">
import type { Suppression, SuppressionReason } from '@sendplane/api'
import { computed, ref } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
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
import { formatDateTime, shortId } from '../lib/format.js'

const { client, t, locale } = useSendplane()
const toast = useToast()
const confirm = useConfirm()

const list = useCursorList<Suppression>((params, signal) =>
  client.get('/api/v1/suppressions', { params: { query: params }, signal }),
)

const adding = ref(false)
const saving = ref(false)
const draft = ref({ email: '', reason: 'manual' as SuppressionReason, expires_at: '' })

const reasonOptions = computed(() =>
  (['hard_bounce', 'complaint', 'manual'] as const).map((reason) => ({
    value: reason,
    label: t(`suppression.reasons.${reason}`),
  })),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'email_norm', label: t('common.email'), width: 'minmax(200px, 2fr)', mono: true },
  { key: 'reason', label: t('common.reason'), width: '140px' },
  {
    key: 'source',
    label: t('suppression.sourceDelivery'),
    width: '150px',
    mono: true,
    secondary: true,
  },
  { key: 'created', label: t('common.created'), width: '190px', secondary: true },
  { key: 'expires', label: t('suppression.expiresAt'), width: '190px', secondary: true },
  { key: 'actions', label: t('common.actions'), width: '90px', align: 'end' },
])

async function add() {
  if (!draft.value.email) return
  saving.value = true
  try {
    await client.put('/api/v1/suppressions/{email}', {
      params: { path: { email: draft.value.email } },
      body: {
        reason: draft.value.reason,
        ...(draft.value.expires_at
          ? { expires_at: new Date(draft.value.expires_at).toISOString() }
          : {}),
      },
    })
    toast.success(t('suppression.added', { email: draft.value.email }))
    adding.value = false
    draft.value = { email: '', reason: 'manual', expires_at: '' }
    list.reset()
  } catch (error) {
    toast.fail(error)
  } finally {
    saving.value = false
  }
}

async function remove(entry: Suppression) {
  if (
    !(await confirm({
      message: t('suppression.confirmDelete', { email: entry.email_norm }),
      danger: true,
    }))
  )
    return
  try {
    await client.del('/api/v1/suppressions/{email}', {
      params: { path: { email: entry.email_norm } },
    })
    toast.success(t('suppression.removed', { email: entry.email_norm }))
    list.reset()
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('suppression.title')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" @click="adding = true">{{ t('suppression.add') }}</SpButton>
      </template>
    </SpPageHeader>

    <SpCard v-if="adding" :title="t('suppression.add')" class="sp-page__block">
      <form class="sp-form-grid" @submit.prevent="add">
        <SpField v-slot="{ id }" :label="t('common.email')" required>
          <SpInput :id="id" v-model="draft.email" type="email" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('common.reason')" required>
          <SpSelect :id="id" v-model="draft.reason" :options="reasonOptions" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('suppression.expiresAt')">
          <SpInput :id="id" v-model="draft.expires_at" type="datetime-local" />
        </SpField>
        <div class="sp-form-grid__actions">
          <SpButton @click="adding = false">{{ t('common.cancel') }}</SpButton>
          <SpButton type="submit" variant="primary" :loading="saving" :disabled="!draft.email">
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
      :row-key="(row: Suppression) => row.email_norm"
      :loading="list.loading.value"
      :caption="t('suppression.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-reason`]="{ row }">
        {{ t(`suppression.reasons.${(row as Suppression).reason}`) }}
      </template>
      <template #[`cell-source`]="{ row }">
        {{ shortId((row as Suppression).source_delivery_id) }}
      </template>
      <template #[`cell-created`]="{ row }">
        {{ formatDateTime((row as Suppression).created_at, locale) }}
      </template>
      <template #[`cell-expires`]="{ row }">
        {{
          (row as Suppression).expires_at
            ? formatDateTime((row as Suppression).expires_at, locale)
            : t('common.never')
        }}
      </template>
      <template #[`cell-actions`]="{ row }">
        <SpButton size="sm" variant="ghost" @click="remove(row as Suppression)">
          {{ t('common.delete') }}
        </SpButton>
      </template>
    </SpTable>
  </div>
</template>
