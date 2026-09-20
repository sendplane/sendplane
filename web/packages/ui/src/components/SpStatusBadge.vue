<script setup lang="ts">
import { computed } from 'vue'

import { useSendplaneOptional } from '../context.js'
import { labelKeyFor, toneFor, type StatusKind, type Tone } from '../lib/status.js'

const props = defineProps<{
  /** Which status vocabulary `value` belongs to. */
  kind: StatusKind
  value: string | undefined
  /** Overrides the translated label, e.g. to append a count. */
  label?: string
  /** Extra context shown as a tooltip, such as `status_reason`. */
  title?: string
}>()

const context = useSendplaneOptional()

const tone = computed<Tone>(() => toneFor(props.kind, props.value))

const text = computed(() => {
  if (props.label) return props.label
  if (!props.value) return '—'
  const key = labelKeyFor(props.kind, props.value)
  const translated = context?.t(key)
  // vue-i18n renders the key itself when a message is missing; showing the raw
  // status value is more useful to an operator than `status.delivery.foo`.
  return !translated || translated === key ? props.value : translated
})
</script>

<template>
  <span class="sp-badge" :class="`sp-badge--${tone}`" :title="title">
    <span class="sp-badge__dot" aria-hidden="true" />
    {{ text }}
  </span>
</template>

<style scoped>
.sp-badge {
  display: inline-flex;
  gap: var(--sp-space-1);
  align-items: center;
  padding: 1px var(--sp-space-2);
  font-size: var(--sp-font-size-sm);
  font-weight: 500;
  line-height: 18px;
  white-space: nowrap;
  border: 1px solid transparent;
  border-radius: 999px;
}

.sp-badge__dot {
  width: 6px;
  height: 6px;
  background: currentcolor;
  border-radius: 50%;
}

.sp-badge--neutral {
  color: var(--sp-neutral);
  background: var(--sp-neutral-soft);
}

.sp-badge--info {
  color: var(--sp-info);
  background: var(--sp-info-soft);
}

.sp-badge--ok {
  color: var(--sp-ok);
  background: var(--sp-ok-soft);
}

.sp-badge--warn {
  color: var(--sp-warn);
  background: var(--sp-warn-soft);
}

.sp-badge--danger {
  color: var(--sp-danger);
  background: var(--sp-danger-soft);
}
</style>
