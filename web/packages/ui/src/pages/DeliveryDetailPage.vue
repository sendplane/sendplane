<script setup lang="ts">
import type { BounceEvent, DeliveryAttempt } from '@sendplane/api'
import { computed } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpEmptyState from '../components/SpEmptyState.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpJsonView from '../components/SpJsonView.vue'
import SpLink from '../components/SpLink.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import { useApiToast } from '../composables/useApiToast.js'
import { useAsync } from '../composables/useAsync.js'
import { useConfirm } from '../composables/useConfirm.js'
import { useSendplane } from '../context.js'
import { formatDateTime, shortId } from '../lib/format.js'

const props = defineProps<{ deliveryId: string }>()

const { client, t, locale } = useSendplane()
const toast = useApiToast()
const confirm = useConfirm()

const delivery = useAsync(
  (signal) =>
    client.get('/api/v1/deliveries/{deliveryId}', {
      params: { path: { deliveryId: props.deliveryId } },
      signal,
    }),
  { watch: () => props.deliveryId },
)

const attempts = useAsync(
  (signal) =>
    client.get('/api/v1/deliveries/{deliveryId}/attempts', {
      params: { path: { deliveryId: props.deliveryId }, query: { limit: 100 } },
      signal,
    }),
  { watch: () => props.deliveryId },
)

const bounces = useAsync(
  (signal) =>
    client.get('/api/v1/deliveries/{deliveryId}/bounces', {
      params: { path: { deliveryId: props.deliveryId }, query: { limit: 50 } },
      signal,
    }),
  { watch: () => props.deliveryId },
)

const timeline = computed<DeliveryAttempt[]>(() =>
  [...(attempts.data.value?.items ?? [])].sort((a, b) => (a.attempt_no ?? 0) - (b.attempt_no ?? 0)),
)

const bounceList = computed<BounceEvent[]>(() => bounces.data.value?.items ?? [])

function durationOf(attempt: DeliveryAttempt): string {
  if (!attempt.started_at || !attempt.finished_at) return '—'
  const ms = new Date(attempt.finished_at).getTime() - new Date(attempt.started_at).getTime()
  return Number.isFinite(ms) ? `${ms} ms` : '—'
}

async function retry() {
  if (!(await confirm(t('delivery.confirmRetryOne')))) return
  try {
    await client.post('/api/v1/deliveries/{deliveryId}/retry', {
      params: { path: { deliveryId: props.deliveryId } },
    })
    toast.success(t('delivery.requeued'))
    await Promise.all([delivery.reload(), attempts.reload()])
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="delivery.data.value?.email ?? t('delivery.one')">
      <template #breadcrumb>
        <SpLink
          v-if="delivery.data.value?.campaign_id"
          :to="{ name: 'campaign', params: { campaignId: delivery.data.value.campaign_id } }"
        >
          ← {{ t('campaign.one') }}
        </SpLink>
      </template>
      <template #badge>
        <SpStatusBadge kind="delivery" :value="delivery.data.value?.status" />
      </template>
      <template #actions>
        <SpButton :loading="delivery.loading.value" @click="delivery.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" @click="retry">{{ t('delivery.retryOne') }}</SpButton>
      </template>
    </SpPageHeader>

    <SpErrorNotice
      :error="delivery.error.value"
      :on-retry="() => delivery.reload()"
      class="sp-page__block"
    />

    <div class="sp-split sp-split--halves sp-page__block">
      <SpCard :title="t('delivery.one')">
        <dl v-if="delivery.data.value" class="sp-detail-list">
          <dt>{{ t('common.id') }}</dt>
          <dd class="sp-mono">{{ delivery.data.value.id }}</dd>
          <dt>{{ t('common.email') }}</dt>
          <dd class="sp-mono">{{ delivery.data.value.email_norm ?? delivery.data.value.email }}</dd>
          <dt>{{ t('delivery.lane') }}</dt>
          <dd>{{ delivery.data.value.lane }}</dd>
          <dt>{{ t('common.locale') }}</dt>
          <dd>{{ delivery.data.value.locale || '—' }}</dd>
          <dt>{{ t('delivery.attempts') }}</dt>
          <dd>
            {{ delivery.data.value.attempt_count ?? 0 }} (gen
            {{ delivery.data.value.retry_gen ?? 0 }})
          </dd>
          <dt>{{ t('delivery.errorClass') }}</dt>
          <dd>
            <SpStatusBadge
              v-if="delivery.data.value.last_error_class"
              kind="errorClass"
              :value="delivery.data.value.last_error_class"
            />
            <span v-else>—</span>
          </dd>
          <dt>{{ t('delivery.lastError') }}</dt>
          <dd>{{ delivery.data.value.last_error || '—' }}</dd>
          <dt>{{ t('delivery.messageId') }}</dt>
          <dd class="sp-mono">{{ delivery.data.value.message_id || '—' }}</dd>
          <dt>{{ t('delivery.sentAt') }}</dt>
          <dd>{{ formatDateTime(delivery.data.value.sent_at, locale) }}</dd>
          <dt>{{ t('delivery.nextAttemptAt') }}</dt>
          <dd>{{ formatDateTime(delivery.data.value.next_attempt_at, locale) }}</dd>
          <dt>{{ t('delivery.firstOpenedAt') }}</dt>
          <dd>{{ formatDateTime(delivery.data.value.first_opened_at, locale) }}</dd>
          <dt>{{ t('delivery.firstClickedAt') }}</dt>
          <dd>{{ formatDateTime(delivery.data.value.first_clicked_at, locale) }}</dd>
          <dt>{{ t('delivery.unsubscribedAt') }}</dt>
          <dd>{{ formatDateTime(delivery.data.value.unsubscribed_at, locale) }}</dd>
        </dl>
        <SpJsonView
          v-if="delivery.data.value?.vars"
          :value="delivery.data.value.vars"
          collapsed
          label="vars"
        />
        <!--
          Only a delivery with no campaign to inherit them from carries its own
          `tenant_vars` (a transactional send or a probe), so this is absent for
          most rows rather than empty.
        -->
        <SpJsonView
          v-if="delivery.data.value?.tenant_vars"
          :value="delivery.data.value.tenant_vars"
          collapsed
          :label="t('tenantVars.title')"
        />
      </SpCard>

      <SpCard :title="t('delivery.attemptTimeline')">
        <SpEmptyState v-if="timeline.length === 0" :title="t('delivery.noAttempts')" />
        <ol v-else class="sp-timeline">
          <li v-for="attempt in timeline" :key="attempt.id" class="sp-timeline__item">
            <div class="sp-timeline__head">
              <strong>#{{ attempt.attempt_no }}</strong>
              <SpStatusBadge
                v-if="attempt.error_class && attempt.error_class !== 'none'"
                kind="errorClass"
                :value="attempt.error_class"
              />
              <span class="sp-muted">{{ formatDateTime(attempt.started_at, locale) }}</span>
            </div>
            <dl class="sp-detail-list sp-timeline__detail">
              <dt>{{ t('delivery.smtpCode') }}</dt>
              <dd class="sp-mono">
                {{ attempt.smtp_code ?? '—' }}
                <template v-if="attempt.enhanced_code">({{ attempt.enhanced_code }})</template>
              </dd>
              <dt>{{ t('delivery.transport') }}</dt>
              <dd class="sp-mono">{{ shortId(attempt.transport_id) }}</dd>
              <dt>{{ t('delivery.duration') }}</dt>
              <dd>{{ durationOf(attempt) }}</dd>
              <template v-if="attempt.error">
                <dt>{{ t('delivery.lastError') }}</dt>
                <dd>{{ attempt.error }}</dd>
              </template>
            </dl>
          </li>
        </ol>
      </SpCard>
    </div>

    <SpCard :title="t('delivery.bounces')">
      <SpEmptyState v-if="bounceList.length === 0" :title="t('delivery.noBounces')" />
      <ul v-else class="sp-bounces">
        <li v-for="bounce in bounceList" :key="bounce.id" class="sp-bounces__item">
          <div class="sp-timeline__head">
            <SpStatusBadge kind="bounce" :value="bounce.type" />
            <span class="sp-mono">{{ bounce.smtp_status || '—' }}</span>
            <span class="sp-muted">{{ t('delivery.source') }}: {{ bounce.source }}</span>
            <SpStatusBadge
              v-if="!bounce.verified"
              kind="delivery"
              value="failed"
              :label="t('delivery.unverified')"
              :title="t('delivery.unverifiedHint')"
            />
            <span class="sp-muted">{{ formatDateTime(bounce.received_at, locale) }}</span>
          </div>
          <p v-if="bounce.diagnostic_code" class="sp-mono sp-bounces__diag">
            {{ bounce.diagnostic_code }}
          </p>
        </li>
      </ul>
    </SpCard>
  </div>
</template>

<style scoped>
.sp-timeline,
.sp-bounces {
  display: flex;
  flex-direction: column;
  gap: var(--sp-space-3);
  margin: 0;
  padding: 0;
  list-style: none;
}

.sp-timeline__item,
.sp-bounces__item {
  padding-left: var(--sp-space-3);
  border-left: 2px solid var(--sp-border-strong);
}

.sp-timeline__head {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-2);
  align-items: center;
}

.sp-timeline__detail {
  margin-top: var(--sp-space-1);
}

.sp-bounces__diag {
  margin: var(--sp-space-1) 0 0;
  overflow-wrap: anywhere;
}
</style>
