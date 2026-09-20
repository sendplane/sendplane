<script setup lang="ts">
defineProps<{ title: string; subtitle?: string }>()
</script>

<template>
  <header class="sp-page-head">
    <div class="sp-page-head__titles">
      <div v-if="$slots.breadcrumb" class="sp-page-head__crumb"><slot name="breadcrumb" /></div>
      <h1 class="sp-page-head__title">
        {{ title }}
        <slot name="badge" />
      </h1>
      <p v-if="subtitle" class="sp-page-head__subtitle">{{ subtitle }}</p>
    </div>
    <div v-if="$slots.actions" class="sp-page-head__actions"><slot name="actions" /></div>
  </header>
</template>

<style scoped>
.sp-page-head {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-3);
  align-items: flex-start;
  justify-content: space-between;
  margin-bottom: var(--sp-space-4);
}

.sp-page-head__crumb {
  margin-bottom: var(--sp-space-1);
  font-size: var(--sp-font-size-sm);
}

.sp-page-head__title {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-2);
  align-items: center;
  margin: 0;
  font-size: 20px;
  font-weight: 650;
}

.sp-page-head__subtitle {
  margin: var(--sp-space-1) 0 0;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}

.sp-page-head__actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-2);
}
</style>
