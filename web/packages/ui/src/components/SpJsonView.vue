<script setup lang="ts">
import { computed } from 'vue'

import { safeJson } from '../lib/format.js'

const props = withDefaults(
  defineProps<{ value: unknown; label?: string; maxHeight?: string; collapsed?: boolean }>(),
  { maxHeight: '320px' },
)

const text = computed(() => safeJson(props.value))
</script>

<template>
  <details v-if="collapsed" class="sp-json">
    <summary>{{ label ?? 'JSON' }}</summary>
    <pre class="sp-json__pre" :style="{ maxHeight }">{{ text }}</pre>
  </details>
  <pre v-else class="sp-json__pre" :style="{ maxHeight }" :aria-label="label">{{ text }}</pre>
</template>

<style scoped>
.sp-json summary {
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
  cursor: pointer;
}

.sp-json__pre {
  margin: var(--sp-space-1) 0 0;
  padding: var(--sp-space-2);
  overflow: auto;
  font-family: var(--sp-font-mono);
  font-size: var(--sp-font-size-sm);
  background: var(--sp-surface-alt);
  border: 1px solid var(--sp-border);
  border-radius: var(--sp-radius-sm);
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
</style>
