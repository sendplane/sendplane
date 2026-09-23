<script setup lang="ts">
import type { ContentMode, Layout } from '@sendplane/api'
import { computed, ref, watch } from 'vue'

import SharedContentNotice from '../components/SharedContentNotice.vue'
import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpCheckbox from '../components/SpCheckbox.vue'
import SpCodeEditor from '../components/SpCodeEditor.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpContentBadges from '../components/SpContentBadges.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import { useApiToast } from '../composables/useApiToast.js'
import { useAsync } from '../composables/useAsync.js'
import { useSendplane } from '../context.js'
import { isSharedReadOnly, isValidContentKey } from '../lib/content.js'

const props = defineProps<{ layoutId?: string }>()

const { client, t, navigate, systemTenant } = useSendplane()
const toast = useApiToast()

const draft = ref<{ name: string; key: string; shared: boolean; mode: ContentMode; body: string }>({
  name: '',
  key: '',
  shared: false,
  mode: 'mjml',
  body: '',
})
const current = ref<Layout | undefined>()
const version = ref<number | undefined>()
const saving = ref(false)

const loaded = useAsync(
  (signal) =>
    props.layoutId
      ? client.get('/api/v1/layouts/{layoutId}', {
          params: { path: { layoutId: props.layoutId } },
          signal,
        })
      : Promise.resolve(undefined as Layout | undefined),
  { watch: () => props.layoutId },
)

watch(loaded.data, (layout) => {
  if (!layout) return
  current.value = layout
  draft.value = {
    name: layout.name,
    key: layout.key ?? '',
    shared: layout.shared === true,
    mode: layout.mode,
    body: layout.body ?? '',
  }
  version.value = layout.version
})

/**
 * A layout the system tenant shares, seen from a tenant (ADR-0018): every
 * write answers `403 platform_read_only`, so the form is shown disabled and
 * the only way forward is an override (`SharedContentNotice`).
 */
const readOnly = computed(() => isSharedReadOnly(current.value, systemTenant.value))

const keyError = computed(() => {
  const key = draft.value.key.trim()
  if (!isValidContentKey(key)) return t('content.keyInvalid')
  if (systemTenant.value && draft.value.shared && !key) return t('content.shareNeedsKey')
  return undefined
})

// A layout is authored as MJML or HTML, never as blocks (spec: LayoutInput).
const modeOptions = computed(() =>
  (['mjml', 'html'] as const).map((mode) => ({ value: mode, label: t(`template.modes.${mode}`) })),
)

const hasContentSlot = computed(() => draft.value.body.includes('content'))

/**
 * PUT replaces the whole layout, so `key` and `shared` are always sent as they
 * stand; an empty key clears it. Only the system tenant may share.
 */
function body() {
  return {
    name: draft.value.name,
    key: draft.value.key.trim(),
    shared: systemTenant.value && draft.value.shared,
    mode: draft.value.mode,
    body: draft.value.body,
  }
}

async function save() {
  saving.value = true
  try {
    if (props.layoutId && version.value !== undefined) {
      await client.put('/api/v1/layouts/{layoutId}', {
        params: { path: { layoutId: props.layoutId } },
        body: { ...body(), version: version.value },
      })
      await loaded.reload()
    } else {
      const created = await client.post('/api/v1/layouts', { body: body() })
      if (created.id) navigate({ name: 'layout', params: { layoutId: created.id } })
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
    <SpPageHeader :title="layoutId ? draft.name || t('layout.one') : t('layout.new')">
      <template #breadcrumb>
        <a class="sp-link" href="#" @click.prevent="navigate({ name: 'layouts' })">
          ← {{ t('layout.title') }}
        </a>
      </template>
      <template #badge>
        <SpContentBadges v-if="current" :item="current" />
      </template>
      <template #actions>
        <!-- A shared layout is read-only in a tenant: override it instead. -->
        <SpButton
          v-if="!readOnly"
          variant="primary"
          :loading="saving"
          :disabled="!draft.name || !!keyError"
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

    <SharedContentNotice kind="layout" :item="current" class="sp-page__block" />

    <SpCard class="sp-page__block">
      <div class="sp-form-grid">
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" :disabled="readOnly" />
        </SpField>
        <SpField
          v-slot="{ id, describedBy }"
          :label="t('content.key')"
          :hint="t('content.keyHint')"
          :error="keyError"
        >
          <SpInput
            :id="id"
            v-model="draft.key"
            class="sp-mono"
            :disabled="readOnly"
            :described-by="describedBy"
          />
        </SpField>
        <SpField v-slot="{ id }" :label="t('layout.mode')">
          <SpSelect :id="id" v-model="draft.mode" :options="modeOptions" :disabled="readOnly" />
        </SpField>
        <!-- Only the system tenant shares; elsewhere the flag is not accepted. -->
        <SpCheckbox
          v-if="systemTenant"
          v-model="draft.shared"
          :label="t('content.share')"
          :hint="t('content.shareHint')"
        />
      </div>
    </SpCard>

    <SpCard :title="t('layout.body')" :subtitle="t('layout.bodyHint')">
      <p v-if="draft.body && !hasContentSlot" class="sp-note sp-note--warn sp-page__block">
        {{ t('layout.bodyHint') }}
      </p>
      <!-- `readonly` is read once when the editor mounts, hence the key. -->
      <SpCodeEditor
        :key="readOnly ? 'read-only' : 'editable'"
        v-model="draft.body"
        :language="draft.mode === 'mjml' ? 'mjml' : 'html'"
        :readonly="readOnly"
        :aria-label="t('layout.body')"
      />
    </SpCard>
  </div>
</template>
