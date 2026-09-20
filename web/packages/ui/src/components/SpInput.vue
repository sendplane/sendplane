<script setup lang="ts">
withDefaults(
  defineProps<{
    modelValue: string | number | undefined
    id?: string
    type?: string
    placeholder?: string
    disabled?: boolean
    readonly?: boolean
    min?: number
    max?: number
    step?: number
    describedBy?: string
    autocomplete?: string
  }>(),
  { type: 'text' },
)

const emit = defineEmits<{ 'update:modelValue': [string] }>()
</script>

<template>
  <input
    :id="id"
    class="sp-input"
    :type="type"
    :value="modelValue ?? ''"
    :placeholder="placeholder"
    :disabled="disabled"
    :readonly="readonly"
    :min="min"
    :max="max"
    :step="step"
    :autocomplete="autocomplete"
    :aria-describedby="describedBy"
    @input="emit('update:modelValue', ($event.target as HTMLInputElement).value)"
  />
</template>

<style scoped>
.sp-input {
  width: 100%;
  min-height: var(--sp-control-height);
  padding: 0 var(--sp-space-2);
  color: var(--sp-text);
  font: inherit;
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius-sm);
}

.sp-input:disabled {
  background: var(--sp-surface-alt);
  opacity: 0.7;
}
</style>
