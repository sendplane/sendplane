<script setup lang="ts">
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    variant?: 'primary' | 'default' | 'danger' | 'ghost'
    size?: 'md' | 'sm'
    type?: 'button' | 'submit' | 'reset'
    disabled?: boolean
    loading?: boolean
    /** Accessible name when the button shows only an icon or a glyph. */
    label?: string
  }>(),
  { variant: 'default', size: 'md', type: 'button' },
)

const emit = defineEmits<{ click: [MouseEvent] }>()

const blocked = computed(() => props.disabled || props.loading)

function onClick(event: MouseEvent) {
  if (blocked.value) {
    event.preventDefault()
    return
  }
  emit('click', event)
}
</script>

<template>
  <button
    class="sp-button"
    :class="[`sp-button--${variant}`, `sp-button--${size}`, { 'is-loading': loading }]"
    :type="type"
    :disabled="blocked"
    :aria-busy="loading ? 'true' : undefined"
    :aria-label="label"
    @click="onClick"
  >
    <span v-if="loading" class="sp-button__spinner" aria-hidden="true" />
    <slot />
  </button>
</template>

<style scoped>
.sp-button {
  display: inline-flex;
  gap: var(--sp-space-2);
  align-items: center;
  justify-content: center;
  min-height: var(--sp-control-height);
  padding: 0 var(--sp-space-3);
  color: var(--sp-text);
  font: inherit;
  font-weight: 500;
  white-space: nowrap;
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius);
  cursor: pointer;
}

.sp-button:hover:not(:disabled) {
  background: var(--sp-surface-hover);
}

.sp-button:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}

.sp-button--sm {
  min-height: 26px;
  padding: 0 var(--sp-space-2);
  font-size: var(--sp-font-size-sm);
}

.sp-button--primary {
  color: #fff;
  background: var(--sp-accent);
  border-color: var(--sp-accent);
}

.sp-button--primary:hover:not(:disabled) {
  background: var(--sp-accent-hover);
  border-color: var(--sp-accent-hover);
}

.sp-button--danger {
  color: #fff;
  background: var(--sp-danger);
  border-color: var(--sp-danger);
}

.sp-button--danger:hover:not(:disabled) {
  background: var(--sp-danger-hover);
  border-color: var(--sp-danger-hover);
}

.sp-button--ghost {
  background: transparent;
  border-color: transparent;
}

.sp-button--ghost:hover:not(:disabled) {
  background: var(--sp-surface-hover);
}

.sp-button__spinner {
  width: 12px;
  height: 12px;
  border: 2px solid currentcolor;
  border-top-color: transparent;
  border-radius: 50%;
  animation: sp-spin 0.7s linear infinite;
}

@keyframes sp-spin {
  to {
    transform: rotate(360deg);
  }
}

@media (prefers-reduced-motion: reduce) {
  .sp-button__spinner {
    animation-duration: 2s;
  }
}
</style>
