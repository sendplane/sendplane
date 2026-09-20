<script setup lang="ts">
import type { TenantSettings, UnsubscribeMode } from '@sendplane/api'
import { computed, ref, watch } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpCheckbox from '../components/SpCheckbox.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import SpTextarea from '../components/SpTextarea.vue'
import { useAsync } from '../composables/useAsync.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatDateTime, splitLines } from '../lib/format.js'

const { client, t, locale } = useSendplane()
const toast = useToast()

const saving = ref(false)
const current = ref<TenantSettings | undefined>()

const draft = ref({
  backoff: '',
  max_attempts: 5,
  retention_days: 90,
  suppression_enabled: true,
  unsubscribe_mode: 'sendplane' as UnsubscribeMode,
  unsubscribe_url_template: '',
  unsubscribe_one_click: false,
  bounce_retain_raw: false,
  default_locale: 'en',
  tracking_domain: '',
  tracking_opens: true,
  tracking_clicks: true,
})

const settings = useAsync((signal) => client.get('/api/v1/settings', { signal }))

watch(settings.data, (value) => {
  if (!value) return
  current.value = value
  draft.value = {
    backoff: (value.retry?.backoff ?? []).join('\n'),
    max_attempts: value.retry?.max_attempts ?? 5,
    retention_days: value.retention_days ?? 90,
    suppression_enabled: value.suppression_enabled !== false,
    unsubscribe_mode: value.unsubscribe_mode ?? 'sendplane',
    unsubscribe_url_template: value.unsubscribe_url_template ?? '',
    unsubscribe_one_click: value.unsubscribe_one_click === true,
    bounce_retain_raw: value.bounce_retain_raw === true,
    default_locale: value.default_locale ?? 'en',
    tracking_domain: value.tracking?.domain ?? '',
    tracking_opens: value.tracking?.opens !== false,
    tracking_clicks: value.tracking?.clicks !== false,
  }
})

const modeOptions = computed(() =>
  (['sendplane', 'host', 'none'] as const).map((mode) => ({
    value: mode,
    label: t(`settings.unsubscribeModes.${mode}`),
  })),
)

// `sendplane` mode rewrites the unsubscribe link through the tracking domain,
// so the domain stops being optional the moment that mode is selected.
const trackingDomainRequired = computed(
  () =>
    draft.value.unsubscribe_mode === 'sendplane' ||
    draft.value.tracking_opens ||
    draft.value.tracking_clicks,
)

const trackingDomainMissing = computed(
  () => trackingDomainRequired.value && !draft.value.tracking_domain,
)

// `unsubscribe_one_click` only matters under `host` mode (spec): under
// `sendplane` the header is always set, and under `none` there is no
// unsubscribe endpoint to declare it for.
const oneClickRelevant = computed(() => draft.value.unsubscribe_mode === 'host')

const signingKeys = computed(() => current.value?.tracking?.signing_keys ?? [])

async function save() {
  if (current.value?.version === undefined) return
  saving.value = true
  try {
    await client.put('/api/v1/settings', {
      body: {
        version: current.value.version,
        retry: {
          backoff: splitLines(draft.value.backoff),
          max_attempts: Number(draft.value.max_attempts),
        },
        retention_days: Number(draft.value.retention_days),
        suppression_enabled: draft.value.suppression_enabled,
        unsubscribe_mode: draft.value.unsubscribe_mode,
        unsubscribe_url_template: draft.value.unsubscribe_url_template,
        unsubscribe_one_click: draft.value.unsubscribe_one_click,
        bounce_retain_raw: draft.value.bounce_retain_raw,
        default_locale: draft.value.default_locale,
        tracking: {
          domain: draft.value.tracking_domain,
          opens: draft.value.tracking_opens,
          clicks: draft.value.tracking_clicks,
        },
      },
    })
    await settings.reload()
    toast.success(t('settings.saved'))
  } catch (error) {
    toast.fail(error)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="sp-page">
    <SpPageHeader :title="t('settings.title')" :subtitle="current?.tenant_id">
      <template #actions>
        <SpButton :loading="settings.loading.value" @click="settings.reload()">
          {{ t('common.refresh') }}
        </SpButton>
        <SpButton variant="primary" :loading="saving" :disabled="!current" @click="save">
          {{ t('common.save') }}
        </SpButton>
      </template>
    </SpPageHeader>

    <SpErrorNotice
      :error="settings.error.value"
      :on-retry="() => settings.reload()"
      class="sp-page__block"
    />

    <SpCard :title="t('settings.retry')" class="sp-page__block">
      <div class="sp-form-grid">
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('settings.backoff')"
          :hint="t('settings.backoffHint')"
        >
          <SpTextarea :id="id" v-model="draft.backoff" mono :rows="5" :described-by="describedBy" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('settings.maxAttempts')"
          :hint="t('settings.maxAttemptsHint')"
        >
          <SpInput
            :id="id"
            v-model="draft.max_attempts"
            type="number"
            :min="1"
            :described-by="describedBy"
          />
        </SpField>
      </div>
    </SpCard>

    <SpCard :title="t('settings.retention')" class="sp-page__block">
      <div class="sp-form-grid">
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('settings.retentionDays')"
          :hint="t('settings.retentionHint')"
        >
          <SpInput
            :id="id"
            v-model="draft.retention_days"
            type="number"
            :min="1"
            :described-by="describedBy"
          />
        </SpField>
        <SpField v-slot="{ id }" :label="t('settings.defaultLocale')">
          <SpInput :id="id" v-model="draft.default_locale" />
        </SpField>
        <SpCheckbox
          v-model="draft.suppression_enabled"
          :label="t('settings.suppressionEnabled')"
          :hint="t('settings.suppressionHint')"
        />
        <SpCheckbox
          v-model="draft.bounce_retain_raw"
          :label="t('settings.bounceRetainRaw')"
          :hint="t('settings.bounceRetainRawHint')"
        />
      </div>
    </SpCard>

    <SpCard :title="t('settings.unsubscribe')" class="sp-page__block">
      <div class="sp-form-grid">
        <SpField v-slot="{ id }" :label="t('settings.unsubscribeMode')">
          <SpSelect :id="id" v-model="draft.unsubscribe_mode" :options="modeOptions" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('settings.unsubscribeUrlTemplate')"
          :hint="t('settings.unsubscribeUrlHint')"
        >
          <SpInput
            :id="id"
            v-model="draft.unsubscribe_url_template"
            :described-by="describedBy"
            placeholder="https://app.example.com/u?e={{ recipient.email | url_encode }}"
          />
        </SpField>
        <SpCheckbox
          v-model="draft.unsubscribe_one_click"
          :disabled="!oneClickRelevant"
          :label="t('settings.oneClick')"
          :hint="t('settings.oneClickHint')"
        />
      </div>
    </SpCard>

    <SpCard :title="t('settings.tracking')">
      <div class="sp-form-grid">
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('settings.trackingDomain')"
          :hint="t('settings.trackingDomainHint')"
          :error="trackingDomainMissing ? t('settings.trackingDomainHint') : undefined"
          :required="trackingDomainRequired"
        >
          <SpInput
            :id="id"
            v-model="draft.tracking_domain"
            placeholder="t.example.com"
            :described-by="describedBy"
          />
        </SpField>
        <SpCheckbox v-model="draft.tracking_opens" :label="t('settings.trackOpens')" />
        <SpCheckbox v-model="draft.tracking_clicks" :label="t('settings.trackClicks')" />
      </div>

      <template v-if="signingKeys.length">
        <h3 class="sp-settings__subhead">{{ t('settings.signingKeys') }}</h3>
        <dl class="sp-detail-list">
          <template v-for="key in signingKeys" :key="key.kid">
            <dt class="sp-mono">{{ key.kid }}</dt>
            <dd>{{ formatDateTime(key.created_at, locale) }}</dd>
          </template>
        </dl>
      </template>
    </SpCard>
  </div>
</template>

<style scoped>
.sp-settings__subhead {
  margin: var(--sp-space-4) 0 var(--sp-space-2);
  font-size: var(--sp-font-size);
}
</style>
