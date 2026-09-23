<script setup lang="ts">
import type { Layout, Template } from '@sendplane/api'
import { computed, ref } from 'vue'

import SpButton from './SpButton.vue'
import { useApiToast } from '../composables/useApiToast.js'
import { useConfirm } from '../composables/useConfirm.js'
import { useSendplane } from '../context.js'
import { isSharedReadOnly } from '../lib/content.js'

/**
 * The editor banner for a template or layout the system tenant shares
 * (ADR-0018), in a tenant's view:
 *
 * - on the shared one itself, which is read-only here, it says so and offers
 *   to override it: `POST .../override` copies it into the tenant under the
 *   same key, and the editor moves to the copy;
 * - on the tenant's override, it names the shared key and offers to delete the
 *   copy, which puts the shared one back in effect, and the editor moves to it.
 *
 * Renders nothing for anything else, including every row in the system
 * tenant, where `shared` is only the flag its author set.
 */
const props = defineProps<{ kind: 'template' | 'layout'; item: Template | Layout | undefined }>()

const { client, t, navigate, systemTenant } = useSendplane()
const toast = useApiToast()
const confirm = useConfirm()

const busy = ref(false)

const readOnly = computed(() => isSharedReadOnly(props.item, systemTenant.value))
const overridden = computed(() => props.item?.overridden === true && !systemTenant.value)
const sharedChanged = computed(
  () =>
    overridden.value &&
    (props.item as Template | undefined)?.shared_updated_since_override === true,
)

const listRoute = computed(() => (props.kind === 'template' ? 'templates' : 'layouts'))

function open(id: string) {
  if (props.kind === 'template') navigate({ name: 'template', params: { templateId: id } })
  else navigate({ name: 'layout', params: { layoutId: id } })
}

async function override() {
  const id = props.item?.id
  if (!id) return
  busy.value = true
  try {
    const copy =
      props.kind === 'template'
        ? await client.post('/api/v1/templates/{templateId}/override', {
            params: { path: { templateId: id } },
          })
        : await client.post('/api/v1/layouts/{layoutId}/override', {
            params: { path: { layoutId: id } },
          })
    toast.success(t('content.overrideCreated', { key: copy.key ?? '' }))
    if (copy.id) open(copy.id)
  } catch (error) {
    toast.fail(error)
  } finally {
    busy.value = false
  }
}

/**
 * The shared original is not listed while the tenant overrides it, so its ID
 * is only knowable once the copy is gone: it is looked up by key afterwards,
 * falling back to the list when it cannot be found (the operator may have
 * stopped sharing it in the meantime).
 */
async function findSharedByKey(key: string): Promise<string | undefined> {
  let cursor: string | undefined
  // A handful of pages is plenty for a console; past that the list will do.
  for (let page = 0; page < 5; page++) {
    const query = { limit: 200, ...(cursor ? { cursor } : {}) }
    const result =
      props.kind === 'template'
        ? await client.get('/api/v1/templates', { params: { query } })
        : await client.get('/api/v1/layouts', { params: { query } })
    const match = result.items.find((item) => item.shared === true && item.key === key)
    if (match?.id) return match.id
    cursor = result.next_cursor
    if (!cursor) return undefined
  }
  return undefined
}

async function revert() {
  const item = props.item
  if (!item?.id) return
  const key = item.key ?? ''
  const ok = await confirm({
    message: t(`content.${props.kind}RevertConfirm`, { key }),
    confirmLabel: t('content.revert'),
    danger: true,
  })
  if (!ok) return
  busy.value = true
  try {
    if (props.kind === 'template') {
      await client.del('/api/v1/templates/{templateId}', {
        params: { path: { templateId: item.id } },
      })
    } else {
      await client.del('/api/v1/layouts/{layoutId}', { params: { path: { layoutId: item.id } } })
    }
    toast.success(t('content.reverted', { key }))
  } catch (error) {
    toast.fail(error)
    busy.value = false
    return
  }
  try {
    const sharedId = key ? await findSharedByKey(key) : undefined
    if (sharedId) open(sharedId)
    else navigate({ name: listRoute.value })
  } catch {
    // The copy is gone either way; the list is the safe place to land.
    navigate({ name: listRoute.value })
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div v-if="readOnly" class="sp-note sp-content-notice" role="status">
    <p class="sp-content-notice__text">{{ t(`content.${kind}ReadOnly`) }}</p>
    <SpButton variant="primary" size="sm" :loading="busy" @click="override">
      {{ t('content.override') }}
    </SpButton>
  </div>
  <div
    v-else-if="overridden"
    class="sp-note sp-content-notice"
    :class="{ 'sp-note--warn': sharedChanged }"
    role="status"
  >
    <div class="sp-content-notice__text">
      <p>{{ t(`content.${kind}OverrideBanner`, { key: item?.key ?? '' }) }}</p>
      <p v-if="sharedChanged">{{ t('content.sharedChangedHint') }}</p>
    </div>
    <SpButton variant="danger" size="sm" :loading="busy" @click="revert">
      {{ t('content.revert') }}
    </SpButton>
  </div>
</template>

<style scoped>
.sp-content-notice {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-3);
  align-items: center;
  justify-content: space-between;
}

.sp-content-notice__text,
.sp-content-notice__text p {
  margin: 0;
}

.sp-content-notice__text {
  display: flex;
  flex: 1 1 280px;
  flex-direction: column;
  gap: var(--sp-space-1);
}
</style>
