<script setup lang="ts">
import { useSendplaneOptional } from '../context.js'

const context = useSendplaneOptional()
const t = (key: string) => context?.t(key) ?? key
</script>

<template>
  <div class="sp-blocks-pending">
    <p class="sp-blocks-pending__title">{{ t('template.blocksPending') }}</p>
    <p class="sp-blocks-pending__hint">{{ t('template.blocksPendingHint') }}</p>
  </div>
</template>

<style scoped>
.sp-blocks-pending {
  display: grid;
  place-content: center;
  gap: var(--sp-space-2);
  min-height: 300px;
  padding: var(--sp-space-5);
  text-align: center;
  background: repeating-linear-gradient(
    45deg,
    var(--sp-surface-alt),
    var(--sp-surface-alt) 10px,
    var(--sp-surface) 10px,
    var(--sp-surface) 20px
  );
  border: 1px dashed var(--sp-border-strong);
  border-radius: var(--sp-radius);
}

.sp-blocks-pending__title {
  margin: 0;
  font-weight: 600;
}

.sp-blocks-pending__hint {
  max-width: 46ch;
  margin: 0;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}
</style>
