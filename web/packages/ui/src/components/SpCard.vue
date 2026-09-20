<script setup lang="ts">
defineProps<{ title?: string; subtitle?: string; padded?: boolean }>()
</script>

<template>
  <section class="sp-card">
    <header v-if="title || $slots.header || $slots.actions" class="sp-card__head">
      <div class="sp-card__titles">
        <h2 v-if="title" class="sp-card__title">{{ title }}</h2>
        <p v-if="subtitle" class="sp-card__subtitle">{{ subtitle }}</p>
        <slot name="header" />
      </div>
      <div v-if="$slots.actions" class="sp-card__actions"><slot name="actions" /></div>
    </header>
    <div class="sp-card__body" :class="{ 'sp-card__body--padded': padded !== false }">
      <slot />
    </div>
    <footer v-if="$slots.footer" class="sp-card__foot"><slot name="footer" /></footer>
  </section>
</template>

<style scoped>
.sp-card {
  background: var(--sp-surface);
  border: 1px solid var(--sp-border);
  border-radius: var(--sp-radius);
  box-shadow: var(--sp-shadow);
}

.sp-card__head {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-2);
  align-items: flex-start;
  justify-content: space-between;
  padding: var(--sp-space-3) var(--sp-space-4);
  border-bottom: 1px solid var(--sp-border);
}

.sp-card__title {
  margin: 0;
  font-size: var(--sp-font-size-lg);
  font-weight: 600;
}

.sp-card__subtitle {
  margin: var(--sp-space-1) 0 0;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}

.sp-card__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-2);
}

.sp-card__body--padded {
  padding: var(--sp-space-4);
}

.sp-card__foot {
  padding: var(--sp-space-3) var(--sp-space-4);
  border-top: 1px solid var(--sp-border);
}
</style>
