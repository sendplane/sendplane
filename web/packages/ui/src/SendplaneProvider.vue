<script setup lang="ts">
import type { SendplaneClient, Whoami } from '@sendplane/api'
import { computed, ref, watch } from 'vue'

import SpConfirmDialog from './components/SpConfirmDialog.vue'
import SpToastHost from './components/SpToastHost.vue'
import { createConfirmApi, provideConfirmApi } from './composables/useConfirm.js'
import { provideToastApi } from './composables/useToast.js'
import {
  provideSendplane,
  type NavigateFn,
  type NavigateTarget,
  type TranslateFn,
} from './context.js'
import type { LocaleMessages } from './i18n/index.js'
import './theme.css'

const props = withDefaults(
  defineProps<{
    client: SendplaneClient
    /** Route-name navigation handled by the host's router. */
    navigate?: NavigateFn
    /** Resolves a route target to an href, so links stay real anchors. */
    href?: (to: NavigateTarget) => string
    locale?: string
    /** vue-i18n messages merged over the bundled `en` / `ko`. */
    messages?: LocaleMessages
    /** Replaces the bundled translator, e.g. with the host's own `t`. */
    t?: TranslateFn
    /**
     * `GET /api/v1/whoami`, which the host loads once the credential is ready.
     * Re-passing it after a tenant switch re-renders the pages that branch on
     * the system-tenant view.
     */
    whoami?: Whoami
    /** Set false when the host already renders toasts and dialogs. */
    overlays?: boolean
  }>(),
  { overlays: true },
)

// The prop is mirrored into a writable ref so the context exposes one locale
// source that both the host (through the prop) and the pages can read.
const localeRef = ref(props.locale ?? 'en')
watch(
  () => props.locale,
  (next) => {
    if (next) localeRef.value = next
  },
)

// Mirrored for the same reason as the locale: the context hands pages one
// reactive source, whether the host re-passes the prop or never passes it.
const whoamiRef = ref<Whoami | undefined>(props.whoami)
watch(
  () => props.whoami,
  (next) => {
    whoamiRef.value = next
  },
)

const context = provideSendplane({
  client: props.client,
  locale: localeRef,
  whoami: whoamiRef,
  ...(props.navigate ? { navigate: props.navigate } : {}),
  ...(props.href ? { href: props.href } : {}),
  ...(props.messages ? { messages: props.messages } : {}),
  ...(props.t ? { t: props.t } : {}),
})

provideToastApi()
const confirmApi = provideConfirmApi(createConfirmApi())

// Named apart from the `locale` prop so the template binding is unambiguous.
const activeLocale = computed(() => context.locale.value)

defineExpose({ context })
</script>

<template>
  <div class="sp-root" :lang="activeLocale">
    <slot :context="context" />
    <template v-if="overlays">
      <SpToastHost />
      <SpConfirmDialog :api="confirmApi" />
    </template>
  </div>
</template>

<style scoped>
.sp-root {
  min-height: 100%;
}
</style>
