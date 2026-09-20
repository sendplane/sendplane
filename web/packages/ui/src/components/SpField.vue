<script setup lang="ts">
import { useId } from 'vue'

defineProps<{
  label: string
  hint?: string
  error?: string
  required?: boolean
  /** Renders the label inline before the control, for checkboxes. */
  inline?: boolean
}>()

/**
 * The generated id is handed to the default slot so every control is bound to
 * its label, whatever element the caller renders.
 */
const id = useId()
</script>

<template>
  <div class="sp-field" :class="{ 'sp-field--inline': inline }">
    <label class="sp-field__label" :for="id">
      {{ label }}
      <span v-if="required" class="sp-field__required" aria-hidden="true">*</span>
    </label>
    <div class="sp-field__control">
      <slot :id="id" :described-by="hint || error ? `${id}-help` : undefined" />
    </div>
    <p v-if="error" :id="`${id}-help`" class="sp-field__error" role="alert">{{ error }}</p>
    <p v-else-if="hint" :id="`${id}-help`" class="sp-field__hint">{{ hint }}</p>
  </div>
</template>

<style scoped>
.sp-field {
  display: flex;
  flex-direction: column;
  gap: var(--sp-space-1);
  min-width: 0;
}

.sp-field--inline {
  flex-direction: row;
  align-items: center;
  gap: var(--sp-space-2);
}

.sp-field--inline .sp-field__label {
  order: 2;
}

.sp-field__label {
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
  font-weight: 600;
}

.sp-field__required {
  color: var(--sp-danger);
}

.sp-field__hint,
.sp-field__error {
  margin: 0;
  font-size: var(--sp-font-size-sm);
}

.sp-field__hint {
  color: var(--sp-text-muted);
}

.sp-field__error {
  color: var(--sp-danger);
}
</style>
