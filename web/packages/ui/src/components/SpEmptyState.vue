<script setup lang="ts">
defineProps<{ title: string; description?: string }>()
</script>

<template>
  <div class="sp-empty">
    <p class="sp-empty__title">{{ title }}</p>
    <p v-if="description" class="sp-empty__description">{{ description }}</p>
    <div v-if="$slots.default" class="sp-empty__actions"><slot /></div>
  </div>
</template>

<style scoped>
.sp-empty {
  padding: var(--sp-space-6) var(--sp-space-4);
  text-align: center;
}

.sp-empty__title {
  margin: 0;
  font-weight: 600;
}

.sp-empty__description {
  margin: var(--sp-space-2) 0 0;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}

.sp-empty__actions {
  margin-top: var(--sp-space-4);
}
</style>
