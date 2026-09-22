<script setup lang="ts">
import { useSendplaneOptional } from '../context.js'

/**
 * Marks a platform resource: one the operator configured rather than a tenant
 * created (ADR-0017). It is read-only everywhere, which is why every list that
 * can contain one shows this instead of leaving the row to look editable.
 */
const context = useSendplaneOptional()
const t = (key: string) => context?.t(key) ?? key
</script>

<template>
  <span class="sp-shared-badge" :title="t('shared.hint')">
    {{ t('shared.badge') }}
  </span>
</template>

<style scoped>
/*
 * Same shape as `SpStatusBadge`, with its own colours: that component's styles
 * are scoped, so the tokens have to be spelled out here rather than borrowed.
 */
.sp-shared-badge {
  display: inline-flex;
  align-items: center;
  padding: 1px var(--sp-space-2);
  color: var(--sp-info);
  font-size: var(--sp-font-size-sm);
  font-weight: 500;
  line-height: 18px;
  white-space: nowrap;
  background: var(--sp-info-soft);
  border: 1px solid transparent;
  border-radius: 999px;
}
</style>
