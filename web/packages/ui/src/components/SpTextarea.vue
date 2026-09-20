<script setup lang="ts">
withDefaults(
  defineProps<{
    modelValue: string | undefined
    id?: string
    rows?: number
    placeholder?: string
    disabled?: boolean
    mono?: boolean
    describedBy?: string
  }>(),
  { rows: 6 },
)

const emit = defineEmits<{ 'update:modelValue': [string] }>()
</script>

<template>
  <textarea
    :id="id"
    class="sp-textarea"
    :class="{ 'sp-mono': mono }"
    :rows="rows"
    :value="modelValue ?? ''"
    :placeholder="placeholder"
    :disabled="disabled"
    :aria-describedby="describedBy"
    @input="emit('update:modelValue', ($event.target as HTMLTextAreaElement).value)"
  />
</template>

<style scoped>
.sp-textarea {
  width: 100%;
  padding: var(--sp-space-2);
  color: var(--sp-text);
  font: inherit;
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius-sm);
  resize: vertical;
}

.sp-textarea.sp-mono {
  font-family: var(--sp-font-mono);
  font-size: var(--sp-font-size-sm);
}
</style>
