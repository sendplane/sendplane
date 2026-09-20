<script setup lang="ts">
import type { TLSMode, Transport } from '@sendplane/api'
import { computed, ref, watch } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import SpStatusBadge from '../components/SpStatusBadge.vue'
import SpTextarea from '../components/SpTextarea.vue'
import { useAsync } from '../composables/useAsync.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime, formatKeyValueLines, parseKeyValueLines } from '../lib/format.js'

const props = defineProps<{ transportId?: string }>()

const { client, t, locale, navigate } = useSendplane()
const toast = useToast()

const draft = ref({
  name: '',
  host: '',
  port: 587,
  tls: 'starttls' as TLSMode,
  username: '',
  password: '',
  max_conns: 4,
  rate_per_second: 0,
})
const domainRates = ref('')
const current = ref<Transport | undefined>()
const saving = ref(false)

const loaded = useAsync(
  (signal) =>
    props.transportId
      ? client.get('/api/v1/transports/{transportId}', {
          params: { path: { transportId: props.transportId } },
          signal,
        })
      : Promise.resolve(undefined as Transport | undefined),
  { watch: () => props.transportId },
)

watch(loaded.data, (transport) => {
  if (!transport) return
  current.value = transport
  draft.value = {
    name: transport.name,
    host: transport.host,
    port: transport.port,
    tls: transport.tls ?? 'starttls',
    username: transport.username ?? '',
    // Never round-trips: the spec marks it writeOnly and answers with `has_password`.
    password: '',
    max_conns: transport.max_conns ?? 4,
    rate_per_second: transport.rate_per_second ?? 0,
  }
  domainRates.value = formatKeyValueLines(transport.domain_rate_per_second)
})

const tlsOptions = computed(() =>
  (['none', 'starttls', 'tls'] as const).map((mode) => ({ value: mode, label: mode })),
)

function body() {
  const rates = parseKeyValueLines(domainRates.value)
  return {
    name: draft.value.name,
    host: draft.value.host,
    port: Number(draft.value.port),
    tls: draft.value.tls,
    username: draft.value.username,
    max_conns: Number(draft.value.max_conns),
    rate_per_second: Number(draft.value.rate_per_second),
    ...(Object.keys(rates).length ? { domain_rate_per_second: rates } : {}),
    ...(draft.value.password ? { password: draft.value.password } : {}),
  }
}

async function save() {
  saving.value = true
  try {
    if (props.transportId && current.value?.version !== undefined) {
      await client.put('/api/v1/transports/{transportId}', {
        params: { path: { transportId: props.transportId } },
        body: { ...body(), version: current.value.version },
      })
      await loaded.reload()
    } else {
      const created = await client.post('/api/v1/transports', { body: body() })
      if (created.id) navigate({ name: 'transport', params: { transportId: created.id } })
    }
    toast.success(t('common.saved'))
  } catch (error) {
    toast.fail(error)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="transportId ? draft.name || t('transport.one') : t('transport.new')">
      <template #breadcrumb>
        <a class="sp-link" href="#" @click.prevent="navigate({ name: 'transports' })">
          ← {{ t('transport.title') }}
        </a>
      </template>
      <template #badge>
        <SpStatusBadge
          v-if="current?.status"
          kind="transport"
          :value="current.status"
          :title="current.status_reason"
        />
      </template>
      <template #actions>
        <SpButton
          variant="primary"
          :loading="saving"
          :disabled="!draft.name || !draft.host"
          @click="save"
        >
          {{ t('common.save') }}
        </SpButton>
      </template>
    </SpPageHeader>

    <SpErrorNotice
      :error="loaded.error.value"
      :on-retry="() => loaded.reload()"
      class="sp-page__block"
    />

    <SpCard class="sp-page__block">
      <form class="sp-form-grid" @submit.prevent="save">
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('transport.host')" required>
          <SpInput :id="id" v-model="draft.host" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('transport.port')" required>
          <SpInput :id="id" v-model="draft.port" type="number" :min="1" :max="65535" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('transport.tls')">
          <SpSelect :id="id" v-model="draft.tls" :options="tlsOptions" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('transport.username')">
          <SpInput :id="id" v-model="draft.username" autocomplete="off" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('transport.password')"
          :hint="current?.has_password ? t('transport.passwordSet') : undefined"
        >
          <SpInput
            :id="id"
            v-model="draft.password"
            type="password"
            autocomplete="new-password"
            :described-by="describedBy"
          />
        </SpField>
        <SpField v-slot="{ id }" :label="t('transport.maxConns')">
          <SpInput :id="id" v-model="draft.max_conns" type="number" :min="1" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('transport.ratePerSecond')"
          :hint="t('transport.rateHint')"
        >
          <SpInput
            :id="id"
            v-model="draft.rate_per_second"
            type="number"
            :min="0"
            :step="0.1"
            :described-by="describedBy"
          />
        </SpField>
      </form>
    </SpCard>

    <SpCard :title="t('transport.domainRates')" class="sp-page__block">
      <SpField
        v-slot="{ id, describedBy }"
        :label="t('transport.domainRates')"
        :hint="t('transport.domainRatesHint')"
      >
        <SpTextarea :id="id" v-model="domainRates" mono :rows="5" :described-by="describedBy" />
      </SpField>
    </SpCard>

    <SpCard v-if="current" :title="t('transport.health')">
      <dl class="sp-detail-list">
        <dt>{{ t('common.status') }}</dt>
        <dd><SpStatusBadge kind="transport" :value="current.status" /></dd>
        <dt>{{ t('transport.statusReason') }}</dt>
        <dd>{{ current.status_reason || '—' }}</dd>
        <dt>{{ t('transport.statusChangedAt') }}</dt>
        <dd>{{ formatDateTime(current.status_changed_at, locale) }}</dd>
      </dl>
    </SpCard>
  </div>
</template>
