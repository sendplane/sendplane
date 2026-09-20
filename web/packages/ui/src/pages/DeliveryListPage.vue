<script setup lang="ts">
import {
  DELIVERY_STATUSES,
  ERROR_CLASSES,
  type Delivery,
  type DeliveryStatus,
  type ErrorClass,
} from '@sendplane/api'
import { computed, ref } from 'vue'

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
 * Deliveries are always read per campaign: the spec has no tenant-wide
 * `/deliveries` list, only `/campaigns/{id}/deliveries`, so the campaign id is
 * a required prop rather than a filter.
 */
const props = defineProps<{ campaignId: string }>()

const { client, t, locale } = useSendplane()

const statuses = ref<string[]>([])
const errorClasses = ref<string[]>([])
const email = ref('')
const emailApplied = ref('')

const list = useCursorList<Delivery>(
  (params, signal) =>
    client.get('/api/v1/campaigns/{campaignId}/deliveries', {
      params: {
        path: { campaignId: props.campaignId },
        query: {
          ...params,
          ...(statuses.value.length ? { status: statuses.value as DeliveryStatus[] } : {}),
          ...(errorClasses.value.length ? { error_class: errorClasses.value as ErrorClass[] } : {}),
          ...(emailApplied.value ? { email: emailApplied.value } : {}),
        },
      },
      signal,
    }),
  { watch: [statuses, errorClasses, emailApplied, () => props.campaignId] },
)

const statusOptions = computed(() =>
  DELIVERY_STATUSES.map((status) => ({ value: status, label: t(`status.delivery.${status}`) })),
)
const errorClassOptions = computed(() =>
  ERROR_CLASSES.map((value) => ({ value, label: t(`status.errorClass.${value}`) })),
)

const columns = computed<TableColumn[]>(() => [
  { key: 'email', label: t('common.email'), width: 'minmax(200px, 2fr)', mono: true },
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

// The filter is an exact match on the normalized address, so it applies on
// submit rather than on every keystroke.
function applyEmail() {
  emailApplied.value = email.value.trim()
}

function clearFilters() {
  statuses.value = []
  errorClasses.value = []
  email.value = ''
  emailApplied.value = ''
}

const filtered = computed(
  () => statuses.value.length > 0 || errorClasses.value.length > 0 || emailApplied.value !== '',
)
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('delivery.title')">
      <template #breadcrumb>
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
      <form @submit.prevent="applyEmail">
        <SpField v-slot="{ id }" :label="t('delivery.searchEmail')">
          <SpInput :id="id" v-model="email" type="email" placeholder="a@x.com" />
        </SpField>
      </form>
      <SpButton @click="applyEmail">{{ t('common.search') }}</SpButton>
    </div>

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
