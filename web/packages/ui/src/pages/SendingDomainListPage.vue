<script setup lang="ts">
import type { SendingDomain } from '@sendplane/api'
import { computed, ref } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTable, { type TableColumn } from '../components/SpTable.vue'
import SpTextarea from '../components/SpTextarea.vue'
import { useConfirm } from '../composables/useConfirm.js'
import { useCursorList } from '../composables/useCursorList.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime, splitLines } from '../lib/format.js'

const { client, t, locale } = useSendplane()
const toast = useToast()
const confirm = useConfirm()

const list = useCursorList<SendingDomain>((params, signal) =>
  client.get('/api/v1/sending-domains', { params: { query: params }, signal }),
)

const editing = ref<SendingDomain | null>(null)
const saving = ref(false)
const draft = ref({
  domain: '',
  dkim_selector: '',
  dkim_private_key: '',
  return_path_domain: '',
  expected_spf: '',
  outbound_ips: '',
})

const columns = computed<TableColumn[]>(() => [
  { key: 'domain', label: t('domain.domain'), width: 'minmax(160px, 2fr)', mono: true },
  { key: 'selector', label: t('domain.dkimSelector'), width: '130px', mono: true },
  { key: 'signing', label: 'DKIM', width: '90px' },
  { key: 'health', label: t('sender.health'), width: '110px' },
  {
    key: 'ips',
    label: t('domain.outboundIps'),
    width: 'minmax(140px, 1fr)',
    mono: true,
    secondary: true,
  },
  { key: 'checked', label: t('sender.checkedAt'), width: '180px', secondary: true },
  { key: 'actions', label: t('common.actions'), width: '150px', align: 'end' },
])

function startCreate() {
  editing.value = {} as SendingDomain
  draft.value = {
    domain: '',
    dkim_selector: '',
    dkim_private_key: '',
    return_path_domain: '',
    expected_spf: '',
    outbound_ips: '',
  }
}

function startEdit(domain: SendingDomain) {
  editing.value = domain
  draft.value = {
    domain: domain.domain,
    dkim_selector: domain.dkim_selector ?? '',
    // writeOnly in the spec: a stored key is reported by `has_dkim_private_key`.
    dkim_private_key: '',
    return_path_domain: domain.return_path_domain ?? '',
    expected_spf: domain.expected_spf ?? '',
    outbound_ips: (domain.outbound_ips ?? []).join('\n'),
  }
}

function body() {
  const ips = splitLines(draft.value.outbound_ips)
  return {
    domain: draft.value.domain,
    ...(draft.value.dkim_selector ? { dkim_selector: draft.value.dkim_selector } : {}),
    ...(draft.value.dkim_private_key ? { dkim_private_key: draft.value.dkim_private_key } : {}),
    ...(draft.value.return_path_domain
      ? { return_path_domain: draft.value.return_path_domain }
      : {}),
    ...(draft.value.expected_spf ? { expected_spf: draft.value.expected_spf } : {}),
    ...(ips.length ? { outbound_ips: ips } : {}),
  }
}

async function save() {
  saving.value = true
  try {
    const target = editing.value
    if (target?.id && target.version !== undefined) {
      await client.put('/api/v1/sending-domains/{domainId}', {
        params: { path: { domainId: target.id } },
        body: { ...body(), version: target.version },
      })
    } else {
      await client.post('/api/v1/sending-domains', { body: body() })
    }
    editing.value = null
    list.reset()
    toast.success(t('common.saved'))
  } catch (error) {
    toast.fail(error)
  } finally {
    saving.value = false
  }
}

async function remove(domain: SendingDomain) {
  if (!domain.id) return
  if (!(await confirm({ message: `${t('common.delete')} ${domain.domain}?`, danger: true }))) return
  try {
    await client.del('/api/v1/sending-domains/{domainId}', {
      params: { path: { domainId: domain.id } },
    })
    list.reset()
  } catch (error) {
    toast.fail(error)
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('domain.title')">
      <template #actions>
        <SpButton :loading="list.loading.value" @click="list.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" @click="startCreate">{{ t('domain.new') }}</SpButton>
      </template>
    </SpPageHeader>

    <SpCard
      v-if="editing"
      :title="editing.id ? editing.domain : t('domain.new')"
      class="sp-page__block"
    >
      <form class="sp-form-grid" @submit.prevent="save">
        <SpField v-slot="{ id }" :label="t('domain.domain')" required>
          <SpInput :id="id" v-model="draft.domain" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('domain.dkimSelector')">
          <SpInput :id="id" v-model="draft.dkim_selector" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('domain.returnPath')">
          <SpInput :id="id" v-model="draft.return_path_domain" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('domain.expectedSpf')">
          <SpInput :id="id" v-model="draft.expected_spf" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('domain.outboundIps')"
          :hint="t('domain.outboundIpsHint')"
        >
          <SpTextarea
            :id="id"
            v-model="draft.outbound_ips"
            mono
            :rows="3"
            :described-by="describedBy"
          />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('domain.dkimKey')"
          :hint="editing.has_dkim_private_key ? t('domain.dkimKeySet') : undefined"
        >
          <SpTextarea
            :id="id"
            v-model="draft.dkim_private_key"
            mono
            :rows="3"
            :described-by="describedBy"
          />
        </SpField>
        <div class="sp-form-grid__actions">
          <SpButton @click="editing = null">{{ t('common.cancel') }}</SpButton>
          <SpButton type="submit" variant="primary" :loading="saving" :disabled="!draft.domain">
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
      :row-key="(row: SendingDomain) => row.id ?? row.domain"
      :loading="list.loading.value"
      :caption="t('domain.title')"
      :has-next="list.hasNext.value"
      :has-previous="list.hasPrevious.value"
      :page-number="list.pageNumber.value"
      @next="list.next()"
      @previous="list.previous()"
    >
      <template #[`cell-selector`]="{ row }">{{
        (row as SendingDomain).dkim_selector || '—'
      }}</template>
      <template #[`cell-signing`]="{ row }">
        {{ (row as SendingDomain).has_dkim_private_key ? 'sendplane' : 'relay' }}
      </template>
      <template #[`cell-health`]="{ row }">
        <SpStatusBadge
          kind="health"
          :value="(row as SendingDomain).health"
          :title="(row as SendingDomain).health_reason"
        />
      </template>
      <template #[`cell-ips`]="{ row }">
        {{ ((row as SendingDomain).outbound_ips ?? []).join(', ') || '—' }}
      </template>
      <template #[`cell-checked`]="{ row }">
        {{ formatDateTime((row as SendingDomain).health_checked_at, locale) }}
      </template>
      <template #[`cell-actions`]="{ row }">
        <div class="sp-actions-row">
          <SpButton size="sm" variant="ghost" @click="startEdit(row as SendingDomain)">
            {{ t('common.edit') }}
          </SpButton>
          <SpButton size="sm" variant="ghost" @click="remove(row as SendingDomain)">
            {{ t('common.delete') }}
          </SpButton>
        </div>
      </template>
    </SpTable>
  </div>
</template>
