<script setup lang="ts">
import { computed, ref } from 'vue'

export interface TabItem {
  value: string
  label: string
  disabled?: boolean
}

const props = defineProps<{ tabs: TabItem[]; modelValue: string; ariaLabel?: string }>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()

const listRef = ref<HTMLElement | null>(null)
const enabled = computed(() => props.tabs.filter((tab) => !tab.disabled))

function select(value: string) {
  if (value !== props.modelValue) emit('update:modelValue', value)
}

/** Roving focus: arrows move between tabs, Home/End jump to the ends. */
function onKeydown(event: KeyboardEvent) {
  const order = enabled.value
  const at = order.findIndex((tab) => tab.value === props.modelValue)
  let nextAt = -1
  if (event.key === 'ArrowRight') nextAt = (at + 1) % order.length
  else if (event.key === 'ArrowLeft') nextAt = (at - 1 + order.length) % order.length
  else if (event.key === 'Home') nextAt = 0
  else if (event.key === 'End') nextAt = order.length - 1
  if (nextAt < 0) return
  event.preventDefault()
  const next = order[nextAt]
  if (!next) return
  select(next.value)
  listRef.value?.querySelector<HTMLElement>(`[data-tab="${next.value}"]`)?.focus()
}
</script>

<template>
  <div ref="listRef" class="sp-tabs" role="tablist" :aria-label="ariaLabel" @keydown="onKeydown">
    <button
      v-for="tab in tabs"
      :key="tab.value"
      role="tab"
      type="button"
      class="sp-tabs__tab"
      :class="{ 'is-active': tab.value === modelValue }"
      :data-tab="tab.value"
      :aria-selected="tab.value === modelValue"
      :tabindex="tab.value === modelValue ? 0 : -1"
      :disabled="tab.disabled"
      @click="select(tab.value)"
    >
      {{ tab.label }}
    </button>
  </div>
</template>

<style scoped>
.sp-tabs {
  display: flex;
  gap: var(--sp-space-1);
  overflow-x: auto;
  border-bottom: 1px solid var(--sp-border);
}

.sp-tabs__tab {
  padding: var(--sp-space-2) var(--sp-space-3);
  color: var(--sp-text-muted);
  font: inherit;
  font-weight: 500;
  white-space: nowrap;
  background: none;
  border: 0;
  border-bottom: 2px solid transparent;
  cursor: pointer;
}

.sp-tabs__tab:hover:not(:disabled) {
  color: var(--sp-text);
}

.sp-tabs__tab.is-active {
  color: var(--sp-text);
  border-bottom-color: var(--sp-accent);
}

.sp-tabs__tab:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
</style>
