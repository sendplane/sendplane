<script setup lang="ts">
import SpSharedBadge from './SpSharedBadge.vue'
import { useSendplaneOptional } from '../context.js'
import type { SharedContent } from '../lib/content.js'

/**
 * The sharing state of a template or layout row (ADR-0018): shared by the
 * system tenant, or this tenant's override of a shared one — plus whether the
 * shared original was published again since the override was copied.
 */
defineProps<{ item: SharedContent }>()

const context = useSendplaneOptional()
const t = (key: string) => context?.t(key) ?? key
</script>

<template>
  <span v-if="item.shared || item.overridden" class="sp-content-badges">
    <SpSharedBadge v-if="item.shared" />
    <span
      v-if="item.overridden"
      class="sp-content-badge sp-content-badge--overridden"
      :title="t('content.overriddenHint')"
    >
      {{ t('content.overridden') }}
    </span>
    <span
      v-if="item.overridden && item.shared_updated_since_override"
      class="sp-content-badge sp-content-badge--changed"
      :title="t('content.sharedChangedHint')"
    >
      {{ t('content.sharedChanged') }}
    </span>
  </span>
</template>

<style scoped>
.sp-content-badges {
  display: inline-flex;
  flex-wrap: wrap;
  gap: var(--sp-space-1);
  align-items: center;
}

/* Same shape as `SpSharedBadge`; its styles are scoped, so they are repeated. */
.sp-content-badge {
  display: inline-flex;
  align-items: center;
  padding: 1px var(--sp-space-2);
  font-size: var(--sp-font-size-sm);
  font-weight: 500;
  line-height: 18px;
  white-space: nowrap;
  border: 1px solid transparent;
  border-radius: 999px;
}

.sp-content-badge--overridden {
  color: var(--sp-text-muted);
  background: var(--sp-surface-alt);
  border-color: var(--sp-border-strong);
}

.sp-content-badge--changed {
  color: var(--sp-warn);
  background: var(--sp-warn-soft);
}
</style>
