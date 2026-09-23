<script setup lang="ts">
import {
  CAMPAIGN_STATUSES,
  type Campaign,
  type CampaignStatus,
  type Sender,
  type TenantVars,
} from '@sendplane/api'
import { computed, ref } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpCheckbox from '../components/SpCheckbox.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpLink from '../components/SpLink.vue'
import SpMultiFilter from '../components/SpMultiFilter.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import SpSharedBadge from '../components/SpSharedBadge.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import TenantVarsEditor from '../components/TenantVarsEditor.vue'
import { useApiToast } from '../composables/useApiToast.js'
import { useAsync } from '../composables/useAsync.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useSendplane } from '../context.js'
import { templateOption, templateReference, templateUseBlocked } from '../lib/content.js'
import { formatDateTime, formatNumber } from '../lib/format.js'
import {
  isSenderUseDenied,
  isTemplateUseDenied,
  isTenantVarsMissing,
  looksTemplated,
  missingTenantVarKeys,
} from '../lib/platform.js'

const { client, t, locale, navigate, systemTenant, tenantId } = useSendplane()
const toast = useApiToast()

const statuses = ref<string[]>([])
const creating = ref(false)
const draft = ref({ name: '', sender_id: '', template_id: '' })
const tenantVars = ref<TenantVars>({})
const missingTenantVars = ref<string[]>([])
const senderUseError = ref('')
const templateUseError = ref('')
// Name the template by key (`template_key`) rather than by ID (ADR-0018).
const byKey = ref(false)
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

const senderList = computed<Sender[]>(() => senders.data.value?.items ?? [])

const senderOptions = computed(() =>
  senderList.value.map((sender) => ({
    value: sender.id ?? '',
    label: sender.shared
      ? `${sender.name} <${sender.from_email}> — ${t('shared.badge')}${
          sender.uses?.length ? ` (${sender.uses.join(', ')})` : ''
        }`
      : `${sender.name} <${sender.from_email}>`,
  })),
)

const selectedSender = computed(() => senderList.value.find((s) => s.id === draft.value.sender_id))

/** Absent `uses` means "anything" — only a shared sender is ever restricted. */
const senderAllowsCampaign = computed(() => {
  const uses = selectedSender.value?.uses
  return !uses || uses.includes('campaign')
})

/** Only a shared sender's From fields are Liquid, so only they get a render. */
const senderTemplates = computed(() => {
  const sender = selectedSender.value
  if (!sender?.shared) return undefined
  if (!looksTemplated(sender.from_name) && !looksTemplated(sender.from_email)) return undefined
  return {
    ...(sender.from_name ? { from_name: sender.from_name } : {}),
    from_email: sender.from_email,
  }
})

/**
 * Own and shared templates (ADR-0018). Unpublished ones stay: a campaign
 * resolves its version at start. A shared template restricted to
 * transactional sends is listed but disabled.
 */
const templateOptions = computed(() =>
  (templates.data.value?.items ?? []).map((template) =>
    templateOption(template, { use: 'campaign', byKey: byKey.value, t }),
  ),
)

const selectedTemplate = computed(() =>
  (templates.data.value?.items ?? []).find((template) => template.id === draft.value.template_id),
)

const templateBlocked = computed(
  () => !!selectedTemplate.value && templateUseBlocked(selectedTemplate.value, 'campaign'),
)

const templateKeyMissing = computed(
  () => byKey.value && !!selectedTemplate.value && !selectedTemplate.value.key,
)

const templateError = computed(() => {
  if (templateBlocked.value) {
    return t('content.templateUseBlocked', {
      uses: (selectedTemplate.value?.uses ?? []).join(', '),
    })
  }
  if (templateKeyMissing.value) return t('content.selectedHasNoKey')
  return templateUseError.value || undefined
})

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
  missingTenantVars.value = []
  senderUseError.value = ''
  templateUseError.value = ''
  try {
    const campaign = await client.post('/api/v1/campaigns', {
      body: {
        name: draft.value.name,
        sender_id: draft.value.sender_id,
        ...(draft.value.template_id
          ? templateReference(selectedTemplate.value, draft.value.template_id, byKey.value)
          : {}),
        ...(Object.keys(tenantVars.value).length ? { tenant_vars: tenantVars.value } : {}),
      },
    })
    creating.value = false
    draft.value = { name: '', sender_id: '', template_id: '' }
    byKey.value = false
    if (campaign.id) navigate({ name: 'campaign', params: { campaignId: campaign.id } })
    else list.reset()
  } catch (error) {
    // These failures name an input on this form, so they render on it.
    if (isTenantVarsMissing(error)) {
      const keys = missingTenantVarKeys(error)
      missingTenantVars.value = keys
      if (keys.length === 0) toast.fail(error)
    } else if (isSenderUseDenied(error)) {
      senderUseError.value = error instanceof Error ? error.message : String(error)
    } else if (isTemplateUseDenied(error)) {
      templateUseError.value = error instanceof Error ? error.message : String(error)
    } else {
      toast.fail(error)
    }
  } finally {
    saving.value = false
  }
}

// `sent` reads the spec's top-level aggregate (MTA-accepted), not
// `by_status.sent`: they differ once a delivery bounces or complains and
// leaves the `sent` status without shrinking the accepted count.
const sentOf = (campaign: Campaign) => campaign.stats?.sent ?? 0
const failedOf = (campaign: Campaign) => campaign.stats?.by_status?.failed ?? 0
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('campaign.title')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <!-- The system tenant cannot create a campaign at all (ADR-0017). -->
        <SpButton v-if="!systemTenant" variant="primary" @click="openCreate">
          {{ t('campaign.new') }}
        </SpButton>
      </template>
    </SpPageHeader>

    <SpCard v-if="creating" :title="t('campaign.new')" class="sp-page__block">
      <form class="sp-form-grid" @submit.prevent="create">
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('campaign.sender')"
          :error="
            !senderAllowsCampaign
              ? t('message.senderUses', { uses: (selectedSender?.uses ?? []).join(', ') })
              : senderUseError || undefined
          "
          required
        >
          <SpSelect
            :id="id"
            v-model="draft.sender_id"
            :options="senderOptions"
            :placeholder="t('common.none')"
            :described-by="describedBy"
          />
        </SpField>
        <SpField v-slot="{ id, describedBy }" :label="t('template.one')" :error="templateError">
          <SpSelect
            :id="id"
            v-model="draft.template_id"
            :options="templateOptions"
            :placeholder="t('common.none')"
            :described-by="describedBy"
          />
        </SpField>
        <SpCheckbox
          v-model="byKey"
          :label="t('content.selectByKey')"
          :hint="t('content.selectByKeyHint')"
        />
        <p v-if="selectedSender?.shared" class="sp-form-grid__note">
          <SpSharedBadge />
          <span class="sp-mono">{{ selectedSender.from_email }}</span>
        </p>
        <div class="sp-form-grid__actions">
          <SpButton @click="creating = false">{{ t('common.cancel') }}</SpButton>
          <SpButton
            type="submit"
            variant="primary"
            :loading="saving"
            :disabled="
              !draft.name ||
              !draft.sender_id ||
              !senderAllowsCampaign ||
              templateBlocked ||
              templateKeyMissing
            "
          >
            {{ t('common.create') }}
          </SpButton>
        </div>
      </form>

      <TenantVarsEditor
        v-model="tenantVars"
        class="sp-page__block"
        :tenant-id="tenantId"
        :missing-keys="missingTenantVars"
        :sender-templates="senderTemplates"
      />
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
        {{ formatNumber(sentOf(row as Campaign), locale) }}
      </template>
      <template #[`cell-failed`]="{ row }">
        {{ formatNumber(failedOf(row as Campaign), locale) }}
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

<style scoped>
.sp-form-grid__note {
  display: flex;
  grid-column: 1 / -1;
  gap: var(--sp-space-2);
  align-items: center;
  margin: 0;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}
</style>
