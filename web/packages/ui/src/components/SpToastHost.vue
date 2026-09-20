<script setup lang="ts">
import { useToast } from '../composables/useToast.js'
import { useSendplaneOptional } from '../context.js'

const { toasts, dismiss } = useToast()
const context = useSendplaneOptional()
const t = (key: string) => context?.t(key) ?? key
</script>

<template>
  <!--
    `aria-live="polite"` rather than `assertive`: an ops console emits a lot of
    confirmations and interrupting the screen reader for each one is hostile.
  -->
  <div class="sp-toasts" role="status" aria-live="polite" aria-atomic="false">
    <div
      v-for="toast in toasts"
      :key="toast.id"
      class="sp-toast"
      :class="`sp-toast--${toast.kind}`"
    >
      <span class="sp-toast__message">{{ toast.message }}</span>
      <button
        type="button"
        class="sp-toast__close"
        :aria-label="t('error.dismiss')"
        @click="dismiss(toast.id)"
      >
        ×
      </button>
    </div>
  </div>
</template>

<style scoped>
.sp-toasts {
  position: fixed;
  right: var(--sp-space-4);
  bottom: var(--sp-space-4);
  z-index: 60;
  display: flex;
  flex-direction: column;
  gap: var(--sp-space-2);
  max-width: min(420px, calc(100vw - 2 * var(--sp-space-4)));
}

.sp-toast {
  display: flex;
  gap: var(--sp-space-2);
  align-items: flex-start;
  padding: var(--sp-space-2) var(--sp-space-3);
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-left-width: 3px;
  border-radius: var(--sp-radius);
  box-shadow: var(--sp-shadow-lg);
}

.sp-toast--success {
  border-left-color: var(--sp-ok);
}

.sp-toast--warning {
  border-left-color: var(--sp-warn);
}

.sp-toast--danger {
  border-left-color: var(--sp-danger);
}

.sp-toast--info {
  border-left-color: var(--sp-accent);
}

.sp-toast__message {
  flex: 1;
  overflow-wrap: anywhere;
}

.sp-toast__close {
  color: var(--sp-text-muted);
  font-size: 18px;
  line-height: 1;
  background: none;
  border: 0;
  cursor: pointer;
}
</style>
