<script setup lang="ts">
import { DELIVERY_STATUSES, type LinkClick } from '@sendplane/api'
import { computed, ref } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpJsonView from '../components/SpJsonView.vue'
import SpLink from '../components/SpLink.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpRecipientUpload from '../components/SpRecipientUpload.vue'
import SpStat from '../components/SpStat.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useAsync } from '../composables/useAsync.js'
import { useConfirm } from '../composables/useConfirm.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime, formatNumber, formatRate } from '../lib/format.js'

const props = defineProps<{ campaignId: string }>()

const { client, t, locale } = useSendplane()
const toast = useToast()
const confirm = useConfirm()

const acting = ref('')

const campaign = useAsync(
  (signal) =>
    client.get('/api/v1/campaigns/{campaignId}', {
      params: { path: { campaignId: props.campaignId } },
      signal,
    }),
  { watch: () => props.campaignId },
)

const links = useAsync(
  (signal) =>
    client.get('/api/v1/campaigns/{campaignId}/links', {
      params: { path: { campaignId: props.campaignId } },
      signal,
    }),
  { watch: () => props.campaignId },
)

const stats = computed(() => campaign.data.value?.stats)
const byStatus = computed(() => stats.value?.by_status ?? {})
const sent = computed(() => byStatus.value.sent ?? 0)

const statusCounts = computed(() =>
  DELIVERY_STATUSES.map((status) => ({ status, count: byStatus.value[status] ?? 0 })).filter(
    (entry) => entry.count > 0 || entry.status === 'sent' || entry.status === 'failed',
  ),
)

const status = computed(() => campaign.data.value?.status)
const canStart = computed(() => status.value === 'draft' || status.value === 'scheduled')
const canPause = computed(() => status.value === 'running')
const canResume = computed(() => status.value === 'paused')
const canCancel = computed(
  () => status.value === 'scheduled' || status.value === 'running' || status.value === 'paused',
)
const canEditRecipients = computed(() => status.value === 'draft')

const linkColumns = computed<TableColumn[]>(() => [
  { key: 'link_no', label: t('campaign.linkNo'), width: '60px', align: 'end' },
  { key: 'url', label: t('campaign.url'), width: 'minmax(220px, 3fr)', mono: true },
  { key: 'clicks', label: t('campaign.clicks'), width: '100px', align: 'end' },
  { key: 'unique_clicks', label: t('campaign.uniqueClicksShort'), width: '100px', align: 'end' },
])

const linkRows = computed<LinkClick[]>(() => links.data.value?.items ?? [])

type LifecycleAction = 'start' | 'pause' | 'resume' | 'cancel'

/**
 * The four lifecycle transitions are separate operations in the spec, so the
 * paths are spelled out rather than built from the action name: that keeps the
 * request body types honest.
 */
function callLifecycle(action: LifecycleAction) {
  const params = { path: { campaignId: props.campaignId } }
  switch (action) {
    case 'start':
      return client.post('/api/v1/campaigns/{campaignId}/start', { params, body: {} })
    case 'pause':
      return client.post('/api/v1/campaigns/{campaignId}/pause', { params })
    case 'resume':
      return client.post('/api/v1/campaigns/{campaignId}/resume', { params })
    case 'cancel':
      return client.post('/api/v1/campaigns/{campaignId}/cancel', { params })
  }
}

async function act(action: LifecycleAction, message: string, successKey: string, danger = false) {
  if (!(await confirm({ message, danger }))) return
  acting.value = action
  try {
    await callLifecycle(action)
    toast.success(t(successKey))
    await campaign.reload()
  } catch (error) {
    toast.fail(error)
  } finally {
    acting.value = ''
  }
}

/** Requeues the failed deliveries; the attempt history is preserved. */
async function retryFailed() {
  if (!(await confirm(t('campaign.confirmRetry')))) return
  acting.value = 'retry'
  try {
    const result = await client.post('/api/v1/campaigns/{campaignId}/retry', {
      params: { path: { campaignId: props.campaignId } },
      body: { status: ['failed'] },
    })
    toast.success(t('campaign.requeued', { count: result.requeued }))
    await campaign.reload()
  } catch (error) {
    toast.fail(error)
  } finally {
    acting.value = ''
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="campaign.data.value?.name ?? t('campaign.one')">
      <template #breadcrumb>
        <SpLink :to="{ name: 'campaigns' }">← {{ t('campaign.title') }}</SpLink>
      </template>
      <template #badge>
        <SpStatusBadge kind="campaign" :value="status" />
      </template>
      <template #actions>
        <SpButton :loading="campaign.loading.value" @click="campaign.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpLink :to="{ name: 'campaign.deliveries', params: { campaignId } }">
          {{ t('campaign.viewDeliveries') }}
        </SpLink>
      </template>
    </SpPageHeader>

    <SpErrorNotice
      :error="campaign.error.value"
      :on-retry="() => campaign.reload()"
      class="sp-page__block"
    />

    <div class="sp-actions-row sp-page__block">
      <SpButton
        v-if="canStart"
        variant="primary"
        :loading="acting === 'start'"
        @click="act('start', t('campaign.confirmStart'), 'campaign.started')"
      >
        {{ t('campaign.start') }}
      </SpButton>
      <SpButton
        v-if="canPause"
        :loading="acting === 'pause'"
        @click="act('pause', t('campaign.confirmPause'), 'campaign.paused')"
      >
        {{ t('campaign.pause') }}
      </SpButton>
      <SpButton
        v-if="canResume"
        variant="primary"
        :loading="acting === 'resume'"
        @click="act('resume', t('campaign.confirmResume'), 'campaign.resumed')"
      >
        {{ t('campaign.resume') }}
      </SpButton>
      <SpButton
        v-if="canCancel"
        variant="danger"
        :loading="acting === 'cancel'"
        @click="act('cancel', t('campaign.confirmCancel'), 'campaign.cancelled', true)"
      >
        {{ t('campaign.cancelSend') }}
      </SpButton>
      <SpButton :loading="acting === 'retry'" @click="retryFailed">
        {{ t('campaign.retryFailed') }}
      </SpButton>
    </div>

    <SpCard
      :title="t('campaign.stats')"
      :subtitle="
        stats?.computed_at
          ? t('campaign.computedAt', { at: formatDateTime(stats.computed_at, locale) })
          : undefined
      "
      class="sp-page__block"
    >
      <div class="sp-stats-grid sp-page__block">
        <SpStat
          v-for="entry in statusCounts"
          :key="entry.status"
          :label="t(`status.delivery.${entry.status}`)"
          :value="formatNumber(entry.count, locale)"
          :tone="entry.status === 'failed' || entry.status === 'bounced' ? 'danger' : 'default'"
        />
      </div>

      <div class="sp-stats-grid">
        <SpStat
          :label="t('campaign.uniqueOpens')"
          :value="formatNumber(stats?.unique_opens, locale)"
          :sub="t('campaign.rateOfSent', { rate: formatRate(stats?.unique_opens, sent, locale) })"
          :hint="t('campaign.opensOverestimated')"
        />
        <SpStat
          :label="t('campaign.uniqueClicks')"
          :value="formatNumber(stats?.unique_clicks, locale)"
          :sub="t('campaign.rateOfSent', { rate: formatRate(stats?.unique_clicks, sent, locale) })"
        />
        <SpStat
          :label="t('campaign.unsubscribed')"
          :value="formatNumber(stats?.unsubscribed, locale)"
          :sub="t('campaign.rateOfSent', { rate: formatRate(stats?.unsubscribed, sent, locale) })"
        />
        <SpStat
          :label="t('campaign.unsubscribeClicked')"
          :value="formatNumber(stats?.unsubscribe_clicked, locale)"
          :hint="t('campaign.unsubscribeClickedHint')"
        />
      </div>

      <p class="sp-note sp-note--warn sp-campaign__opens-note">
        {{ t('campaign.opensOverestimated') }}
      </p>
    </SpCard>

    <div class="sp-split sp-split--halves sp-page__block">
      <SpCard :title="t('campaign.one')">
        <dl v-if="campaign.data.value" class="sp-detail-list">
          <dt>{{ t('common.id') }}</dt>
          <dd class="sp-mono">{{ campaign.data.value.id }}</dd>
          <dt>{{ t('campaign.sender') }}</dt>
          <dd class="sp-mono">{{ campaign.data.value.sender_id }}</dd>
          <dt>{{ t('campaign.messageVersion') }}</dt>
          <dd class="sp-mono">{{ campaign.data.value.version_id ?? '—' }}</dd>
          <dt>{{ t('campaign.defaultLocale') }}</dt>
          <dd>{{ campaign.data.value.default_locale || '—' }}</dd>
          <dt>{{ t('campaign.scheduleAt') }}</dt>
          <dd>{{ formatDateTime(campaign.data.value.schedule_at, locale) }}</dd>
          <dt>{{ t('campaign.startedAt') }}</dt>
          <dd>{{ formatDateTime(campaign.data.value.started_at, locale) }}</dd>
          <dt>{{ t('campaign.completedAt') }}</dt>
          <dd>{{ formatDateTime(campaign.data.value.completed_at, locale) }}</dd>
        </dl>
        <SpJsonView
          v-if="campaign.data.value?.vars"
          :value="campaign.data.value.vars"
          collapsed
          :label="t('campaign.vars')"
        />
      </SpCard>

      <SpRecipientUpload
        :campaign-id="campaignId"
        :disabled="!canEditRecipients"
        @ingested="campaign.reload()"
      />
    </div>

    <SpCard :title="t('campaign.links')" :padded="false">
      <SpTable
        :columns="linkColumns"
        :rows="linkRows"
        :row-key="(row: LinkClick) => `${row.link_no}:${row.url}`"
        :loading="links.loading.value"
        :caption="t('campaign.links')"
        :paginated="false"
        :empty-title="t('campaign.noLinks')"
      >
        <template #[`cell-clicks`]="{ row }">
          {{ formatNumber((row as LinkClick).clicks, locale) }}
        </template>
        <template #[`cell-unique_clicks`]="{ row }">
          {{ formatNumber((row as LinkClick).unique_clicks, locale) }}
        </template>
      </SpTable>
    </SpCard>
  </div>
</template>

<style scoped>
.sp-campaign__opens-note {
  margin-top: var(--sp-space-3);
}
</style>
