<script setup lang="ts">
import type { ContentMode, Layout } from '@sendplane/api'
import { computed, ref, watch } from 'vue'

import SpButton from '../components/SpButton.vue'
import SpCard from '../components/SpCard.vue'
import SpCodeEditor from '../components/SpCodeEditor.vue'
import SpErrorNotice from '../components/SpErrorNotice.vue'
import SpField from '../components/SpField.vue'
import SpInput from '../components/SpInput.vue'
import SpPageHeader from '../components/SpPageHeader.vue'
import SpSelect from '../components/SpSelect.vue'
import { useAsync } from '../composables/useAsync.js'
import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'

const props = defineProps<{ layoutId?: string }>()

const { client, t, navigate } = useSendplane()
const toast = useToast()

const draft = ref<{ name: string; mode: ContentMode; body: string }>({
  name: '',
  mode: 'mjml',
  body: '',
})
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
  draft.value = { name: layout.name, mode: layout.mode, body: layout.body ?? '' }
  version.value = layout.version
})

// A layout is authored as MJML or HTML, never as blocks (spec: LayoutInput).
const modeOptions = computed(() =>
  (['mjml', 'html'] as const).map((mode) => ({ value: mode, label: t(`template.modes.${mode}`) })),
)

const hasContentSlot = computed(() => draft.value.body.includes('content'))

async function save() {
  saving.value = true
  try {
    if (props.layoutId && version.value !== undefined) {
      await client.put('/api/v1/layouts/{layoutId}', {
        params: { path: { layoutId: props.layoutId } },
        body: { ...draft.value, version: version.value },
      })
      await loaded.reload()
    } else {
      const created = await client.post('/api/v1/layouts', { body: draft.value })
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
      <template #actions>
        <SpButton variant="primary" :loading="saving" :disabled="!draft.name" @click="save">
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
      <div class="sp-form-grid">
        <SpField v-slot="{ id }" :label="t('common.name')" required>
          <SpInput :id="id" v-model="draft.name" />
        </SpField>
        <SpField v-slot="{ id }" :label="t('layout.mode')">
          <SpSelect :id="id" v-model="draft.mode" :options="modeOptions" />
        </SpField>
      </div>
    </SpCard>

    <SpCard :title="t('layout.body')" :subtitle="t('layout.bodyHint')">
      <p v-if="draft.body && !hasContentSlot" class="sp-note sp-note--warn sp-page__block">
        {{ t('layout.bodyHint') }}
      </p>
      <SpCodeEditor
        v-model="draft.body"
        :language="draft.mode === 'mjml' ? 'mjml' : 'html'"
        :aria-label="t('layout.body')"
      />
    </SpCard>
  </div>
</template>
