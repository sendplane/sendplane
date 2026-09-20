<script setup lang="ts">
import { nextTick, ref, watch } from 'vue'

import type { ConfirmApi } from '../composables/useConfirm.js'
import { useSendplaneOptional } from '../context.js'
import SpButton from './SpButton.vue'

const props = defineProps<{ api: ConfirmApi }>()

const context = useSendplaneOptional()
const t = (key: string) => context?.t(key) ?? key

const confirmRef = ref<InstanceType<typeof SpButton> | null>(null)

// Focus lands on the confirm button when the dialog opens, and the dialog traps
// Escape so a destructive action is never one stray keypress away.
watch(
  () => props.api.current.value,
  async (request) => {
    if (!request) return
    await nextTick()
    ;(confirmRef.value?.$el as HTMLElement | undefined)?.focus()
  },
)

function onKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') {
    event.stopPropagation()
    props.api.current.value?.resolve(false)
  }
}
</script>

<template>
  <div
    v-if="api.current.value"
    class="sp-dialog__backdrop"
    @click.self="api.current.value?.resolve(false)"
  >
    <div
      class="sp-dialog"
      role="alertdialog"
      aria-modal="true"
      :aria-label="api.current.value.title ?? t('common.confirm')"
      @keydown="onKeydown"
    >
      <h2 v-if="api.current.value.title" class="sp-dialog__title">{{ api.current.value.title }}</h2>
      <p class="sp-dialog__message">{{ api.current.value.message }}</p>
      <div class="sp-dialog__actions">
        <SpButton @click="api.current.value?.resolve(false)">
          {{ api.current.value.cancelLabel ?? t('common.cancel') }}
        </SpButton>
        <SpButton
          ref="confirmRef"
          :variant="api.current.value.danger ? 'danger' : 'primary'"
          @click="api.current.value?.resolve(true)"
        >
          {{ api.current.value.confirmLabel ?? t('common.confirm') }}
        </SpButton>
      </div>
    </div>
  </div>
</template>

<style scoped>
.sp-dialog__backdrop {
  position: fixed;
  inset: 0;
  z-index: 70;
  display: grid;
  place-items: center;
  padding: var(--sp-space-4);
  background: var(--sp-overlay);
}

.sp-dialog {
  width: min(460px, 100%);
  padding: var(--sp-space-4);
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius);
  box-shadow: var(--sp-shadow-lg);
}

.sp-dialog__title {
  margin: 0 0 var(--sp-space-2);
  font-size: var(--sp-font-size-lg);
}

.sp-dialog__message {
  margin: 0;
}

.sp-dialog__actions {
  display: flex;
  gap: var(--sp-space-2);
  justify-content: flex-end;
  margin-top: var(--sp-space-4);
}
</style>
