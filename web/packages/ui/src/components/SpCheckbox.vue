<script setup lang="ts">
import { useId } from 'vue'

defineProps<{ modelValue: boolean | undefined; label: string; hint?: string; disabled?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [boolean] }>()

const id = useId()
</script>

<template>
  <div class="sp-check">
    <input
      :id="id"
      type="checkbox"
      :checked="modelValue === true"
      :disabled="disabled"
      :aria-describedby="hint ? `${id}-hint` : undefined"
      @change="emit('update:modelValue', ($event.target as HTMLInputElement).checked)"
    />
    <label :for="id">{{ label }}</label>
    <p v-if="hint" :id="`${id}-hint`" class="sp-check__hint">{{ hint }}</p>
  </div>
</template>

<style scoped>
.sp-check {
  display: grid;
  grid-template-columns: auto 1fr;
  gap: var(--sp-space-1) var(--sp-space-2);
  align-items: center;
}

.sp-check label {
  font-weight: 500;
}

.sp-check__hint {
  grid-column: 2;
  margin: 0;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}
</style>
