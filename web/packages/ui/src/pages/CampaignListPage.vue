<script setup lang="ts">
import { CAMPAIGN_STATUSES, type Campaign, type CampaignStatus } from '@sendplane/api'
import { computed, ref } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpLink from '../components/SpLink.vue'
import SpMultiFilter from '../components/SpMultiFilter.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useAsync } from '../composables/useAsync.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime, formatNumber } from '../lib/format.js'

const { client, t, locale, navigate } = useSendplane()
const toast = useToast()

const statuses = ref<string[]>([])
const creating = ref(false)
const draft = ref({ name: '', sender_id: '', template_id: '' })
const saving = ref(false)

const list = useCursorList<Campaign>(
  (params, signal) =>
    client.get('/api/v1/campaigns', {
      params: {
        query: {
          ...params,
          ...(statuses.value.length ? { status: statuses.value as CampaignStatus[] } : {}),
        },
      },
      signal,
    }),
  { watch: statuses },
)

// Senders and templates are small lists; loading them up front keeps the create
// form a single screen instead of a wizard.
const senders = useAsync(
  (signal) => client.get('/api/v1/senders', { params: { query: { limit: 200 } }, signal }),
  { immediate: false },
)
const templates = useAsync(
  (signal) => client.get('/api/v1/templates', { params: { query: { limit: 200 } }, signal }),
  { immediate: false },
)

const statusOptions = computed(() =>
  CAMPAIGN_STATUSES.map((status) => ({ value: status, label: t(`status.campaign.${status}`) })),
)

const senderOptions = computed(() =>
  (senders.data.value?.items ?? []).map((sender) => ({
    value: sender.id ?? '',
    label: `${sender.name} <${sender.from_email}>`,
  })),
)

const templateOptions = computed(() =>
  (templates.data.value?.items ?? []).map((template) => ({
    value: template.id ?? '',
    label: template.name,
  })),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('common.name'), width: 'minmax(180px, 2fr)' },
  { key: 'status', label: t('common.status'), width: '120px' },
  { key: 'sent', label: t('status.delivery.sent'), width: '90px', align: 'end' },
  { key: 'failed', label: t('status.delivery.failed'), width: '90px', align: 'end' },
  { key: 'schedule', label: t('campaign.scheduleAt'), width: '190px', secondary: true },
  { key: 'updated', label: t('common.updated'), width: '190px', secondary: true },
])

function openCreate() {
  creating.value = true
  void senders.reload()
  void templates.reload()
}

async function create() {
  if (!draft.value.name || !draft.value.sender_id) return
  saving.value = true
  try {
    const campaign = await client.post('/api/v1/campaigns', {
      body: {
        name: draft.value.name,
        sender_id: draft.value.sender_id,
        ...(draft.value.template_id ? { template_id: draft.value.template_id } : {}),
      },
    })
    creating.value = false
    draft.value = { name: '', sender_id: '', template_id: '' }
    if (campaign.id) navigate({ name: 'campaign', params: { campaignId: campaign.id } })
    else list.reset()
  } catch (error) {
    toast.fail(error)
  } finally {
    saving.value = false
  }
}

const countOf = (campaign: Campaign, status: string) => campaign.stats?.by_status?.[status] ?? 0
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('campaign.title')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" @click="openCreate">{{ t('campaign.new') }}</SpButton>
      </template>
    </SpPageHeader>

    <SpCard v-if="creating" :title="t('campaign.new')" class="sp-page__block">
      <form class="sp-form-grid" @submit.prevent="create">
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('campaign.sender')" required>
          <SpSelect
            :id="id"
            v-model="draft.sender_id"
            :options="senderOptions"
            :placeholder="t('common.none')"
          />
        </SpField>
        <SpField v-slot="{ id }" :label="t('template.one')">
          <SpSelect
            :id="id"
            v-model="draft.template_id"
            :options="templateOptions"
            :placeholder="t('common.none')"
          />
        </SpField>
        <div class="sp-form-grid__actions">
          <SpButton @click="creating = false">{{ t('common.cancel') }}</SpButton>
          <SpButton
            type="submit"
            variant="primary"
            :loading="saving"
            :disabled="!draft.name || !draft.sender_id"
          >
            {{ t('common.create') }}
          </SpButton>
        </div>
      </form>
    </SpCard>

    <SpMultiFilter
      v-model="statuses"
      class="sp-page__block"
      :legend="t('common.status')"
      :options="statusOptions"
    />

    <SpErrorNotice
      :error="list.error.value"
      :on-retry="() => list.reload()"
      class="sp-page__block"
    />

    <SpTable
      :columns="columns"
      :rows="list.items.value"
      :row-key="(row: Campaign) => row.id ?? row.name"
      :loading="list.loading.value"
      :caption="t('campaign.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      :empty-title="statuses.length ? t('empty.noResults') : t('empty.nothing')"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-name`]="{ row }">
        <SpLink :to="{ name: 'campaign', params: { campaignId: (row as Campaign).id! } }">
          {{ (row as Campaign).name }}
        </SpLink>
      </template>
      <template #[`cell-status`]="{ row }">
        <SpStatusBadge kind="campaign" :value="(row as Campaign).status" />
      </template>
      <template #[`cell-sent`]="{ row }">
        {{ formatNumber(countOf(row as Campaign, 'sent'), locale) }}
      </template>
      <template #[`cell-failed`]="{ row }">
        {{ formatNumber(countOf(row as Campaign, 'failed'), locale) }}
      </template>
      <template #[`cell-schedule`]="{ row }">
        {{ formatDateTime((row as Campaign).schedule_at ?? (row as Campaign).started_at, locale) }}
      </template>
      <template #[`cell-updated`]="{ row }">
        {{ formatDateTime((row as Campaign).updated_at, locale) }}
      </template>
    </SpTable>
  </div>
</template>
