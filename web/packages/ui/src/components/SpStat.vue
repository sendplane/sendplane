<script setup lang="ts">
defineProps<{
  label: string
  value: string
  /** Secondary line, typically a rate against `sent`. */
  sub?: string
  /** Explains a caveat, shown as a tooltip and read by screen readers. */
  hint?: string
  tone?: 'default' | 'ok' | 'warn' | 'danger'
}>()
</script>

<template>
  <div class="sp-stat" :class="tone ? `sp-stat--${tone}` : undefined">
    <div class="sp-stat__label">
      {{ label }}
      <abbr v-if="hint" class="sp-stat__hint" :title="hint">?</abbr>
    </div>
    <div class="sp-stat__value">{{ value }}</div>
    <div v-if="sub" class="sp-stat__sub">{{ sub }}</div>
  </div>
</template>

<style scoped>
.sp-stat {
  min-width: 0;
  padding: var(--sp-space-3);
  background: var(--sp-surface);
  border: 1px solid var(--sp-border);
  border-radius: var(--sp-radius);
}

.sp-stat__label {
  display: flex;
  gap: var(--sp-space-1);
  align-items: center;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
  font-weight: 600;
}

.sp-stat__hint {
  display: inline-grid;
  place-items: center;
  width: 14px;
  height: 14px;
  font-size: 10px;
  text-decoration: none;
  background: var(--sp-surface-alt);
  border: 1px solid var(--sp-border-strong);
  border-radius: 50%;
  cursor: help;
}

.sp-stat__value {
  margin-top: var(--sp-space-1);
  font-size: 22px;
  font-variant-numeric: tabular-nums;
  font-weight: 600;
}

.sp-stat--ok .sp-stat__value {
  color: var(--sp-ok);
}

.sp-stat--warn .sp-stat__value {
  color: var(--sp-warn);
}

.sp-stat--danger .sp-stat__value {
  color: var(--sp-danger);
}

.sp-stat__sub {
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}
</style>
