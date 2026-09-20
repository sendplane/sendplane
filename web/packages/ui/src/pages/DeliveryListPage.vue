<script setup lang="ts">
import {
  DELIVERY_STATUSES,
  ERROR_CLASSES,
  LANES,
  type Delivery,
  type DeliveryStatus,
  type ErrorClass,
  type Lane,
} from '@sendplane/api'
import { computed, ref, watch } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpLink from '../components/SpLink.vue'
import SpMultiFilter from '../components/SpMultiFilter.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import { useCursorList } from '../composables/useCursorList.js'
import { useSendplane } from '../context.js'
import { formatDateTime } from '../lib/format.js'

/**
 * Deliveries are read tenant-wide through `GET /api/v1/deliveries`
 * (`client.listDeliveries`). `campaignId` is an optional prop rather than a
 * required one: the campaign-scoped link from `CampaignDetailPage` passes it
 * to pre-fill the campaign filter, but the top-level "Deliveries" nav entry
 * mounts this page with none, listing every delivery of the tenant.
 */
const props = defineProps<{ campaignId?: string }>()

const { client, t, locale } = useSendplane()

const campaignIdFilter = ref(props.campaignId ?? '')
const campaignIdApplied = ref(props.campaignId ?? '')
const lanes = ref<string[]>([])
const statuses = ref<string[]>([])
const errorClasses = ref<string[]>([])
const email = ref('')
const emailApplied = ref('')
const since = ref('')
const sinceApplied = ref('')
const until = ref('')
const untilApplied = ref('')

// A campaign navigating in later (or the host swapping the prop) re-seeds the
// filter, same as a fresh mount would.
watch(
  () => props.campaignId,
  (campaignId) => {
    campaignIdFilter.value = campaignId ?? ''
    campaignIdApplied.value = campaignId ?? ''
  },
)

const list = useCursorList<Delivery>(
  (params, signal) =>
    client.listDeliveries(
      {
        ...params,
        ...(campaignIdApplied.value ? { campaign_id: campaignIdApplied.value } : {}),
        ...(lanes.value.length ? { lane: lanes.value as Lane[] } : {}),
        ...(statuses.value.length ? { status: statuses.value as DeliveryStatus[] } : {}),
        ...(errorClasses.value.length ? { error_class: errorClasses.value as ErrorClass[] } : {}),
        ...(emailApplied.value ? { email: emailApplied.value } : {}),
        ...(sinceApplied.value ? { since: toInstant(sinceApplied.value) } : {}),
        ...(untilApplied.value ? { until: toInstant(untilApplied.value) } : {}),
      },
      signal,
    ),
  {
    watch: [
      campaignIdApplied,
      lanes,
      statuses,
      errorClasses,
      emailApplied,
      sinceApplied,
      untilApplied,
    ],
  },
)

const laneOptions = computed(() => LANES.map((lane) => ({ value: lane, label: lane })))
const statusOptions = computed(() =>
  DELIVERY_STATUSES.map((status) => ({ value: status, label: t(`status.delivery.${status}`) })),
)
const errorClassOptions = computed(() =>
  ERROR_CLASSES.map((value) => ({ value, label: t(`status.errorClass.${value}`) })),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'email', label: t('common.email'), width: 'minmax(200px, 2fr)', mono: true },
  { key: 'lane', label: t('delivery.lane'), width: '110px' },
  { key: 'status', label: t('common.status'), width: '120px' },
  { key: 'error_class', label: t('delivery.errorClass'), width: '120px' },
  { key: 'attempt_count', label: t('delivery.attempts'), width: '90px', align: 'end' },
  {
    key: 'last_error',
    label: t('delivery.lastError'),
    width: 'minmax(160px, 2fr)',
    secondary: true,
  },
  { key: 'updated', label: t('common.updated'), width: '180px', secondary: true },
])

// The email, campaign, since and until filters are exact matches, so they
// apply on submit rather than on every keystroke.
function applyFilters() {
  campaignIdApplied.value = campaignIdFilter.value.trim()
  emailApplied.value = email.value.trim()
  sinceApplied.value = since.value.trim()
  untilApplied.value = until.value.trim()
}

function clearFilters() {
  campaignIdFilter.value = ''
  campaignIdApplied.value = ''
  lanes.value = []
  statuses.value = []
  errorClasses.value = []
  email.value = ''
  emailApplied.value = ''
  since.value = ''
  sinceApplied.value = ''
  until.value = ''
  untilApplied.value = ''
}

/** `datetime-local` has no timezone; the API wants an instant. */
function toInstant(localValue: string): string {
  const date = new Date(localValue)
  return Number.isNaN(date.getTime()) ? localValue : date.toISOString()
}

const filtered = computed(
  () =>
    campaignIdApplied.value !== '' ||
    lanes.value.length > 0 ||
    statuses.value.length > 0 ||
    errorClasses.value.length > 0 ||
    emailApplied.value !== '' ||
    sinceApplied.value !== '' ||
    untilApplied.value !== '',
)
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('delivery.title')">
      <template v-if="campaignId" #breadcrumb>
        <SpLink :to="{ name: 'campaign', params: { campaignId } }"
          >← {{ t('campaign.one') }}</SpLink
        >
      </template>
      <template #actions>
        <SpButton v-if="filtered" @click="clearFilters">{{ t('common.clear') }}</SpButton>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
      </template>
    </SpPageHeader>

    <div class="sp-toolbar">
      <SpMultiFilter v-model="lanes" :legend="t('delivery.filterLane')" :options="laneOptions" />
      <SpMultiFilter
        v-model="statuses"
        :legend="t('delivery.filterStatus')"
        :options="statusOptions"
      />
      <SpMultiFilter
        v-model="errorClasses"
        :legend="t('delivery.filterErrorClass')"
        :options="errorClassOptions"
      />
    </div>

    <form class="sp-toolbar sp-page__block" @submit.prevent="applyFilters">
      <SpField v-slot="{ id }" :label="t('delivery.filterCampaign')">
        <SpInput :id="id" v-model="campaignIdFilter" placeholder="c1" />
      </SpField>
      <SpField v-slot="{ id }" :label="t('delivery.searchEmail')">
        <SpInput :id="id" v-model="email" type="email" placeholder="a@x.com" />
      </SpField>
      <SpField v-slot="{ id }" :label="t('delivery.filterSince')">
        <SpInput :id="id" v-model="since" type="datetime-local" />
      </SpField>
      <SpField v-slot="{ id }" :label="t('delivery.filterUntil')">
        <SpInput :id="id" v-model="until" type="datetime-local" />
      </SpField>
      <SpButton type="submit">{{ t('common.search') }}</SpButton>
    </form>

    <SpErrorNotice
      :error="list.error.value"
      :on-retry="() => list.reload()"
      class="sp-page__block"
    />

    <SpTable
      :columns="columns"
      :rows="list.items.value"
      :row-key="(row: Delivery) => row.id"
      :loading="list.loading.value"
      :caption="t('delivery.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      :empty-title="filtered ? t('empty.noResults') : t('empty.nothing')"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-email`]="{ row }">
        <SpLink :to="{ name: 'delivery', params: { deliveryId: (row as Delivery).id } }">
          {{ (row as Delivery).email }}
        </SpLink>
      </template>
      <template #[`cell-lane`]="{ row }">{{ (row as Delivery).lane }}</template>
      <template #[`cell-status`]="{ row }">
        <SpStatusBadge kind="delivery" :value="(row as Delivery).status" />
      </template>
      <template #[`cell-error_class`]="{ row }">
        <SpStatusBadge
          v-if="(row as Delivery).last_error_class && (row as Delivery).last_error_class !== 'none'"
          kind="errorClass"
          :value="(row as Delivery).last_error_class"
        />
        <span v-else>—</span>
      </template>
      <template #[`cell-last_error`]="{ row }">{{ (row as Delivery).last_error || '—' }}</template>
      <template #[`cell-updated`]="{ row }">
        {{ formatDateTime((row as Delivery).updated_at, locale) }}
      </template>
    </SpTable>
  </div>
</template>
