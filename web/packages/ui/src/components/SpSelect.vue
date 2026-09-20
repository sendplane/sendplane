<script setup lang="ts">
export interface SelectOption {
  value: string
  label: string
  disabled?: boolean
}

defineProps<{
  modelValue: string | undefined
  options: SelectOption[]
  id?: string
  disabled?: boolean
  describedBy?: string
  /** Label of the leading blank option; omit to require a choice. */
  placeholder?: string
}>()

const emit = defineEmits<{ 'update:modelValue': [string] }>()
</script>

<template>
  <select
    :id="id"
    class="sp-select"
    :value="modelValue ?? ''"
    :disabled="disabled"
    :aria-describedby="describedBy"
    @change="emit('update:modelValue', ($event.target as HTMLSelectElement).value)"
  >
    <option v-if="placeholder !== undefined" value="">{{ placeholder }}</option>
    <option
      v-for="option in options"
      :key="option.value"
      :value="option.value"
      :disabled="option.disabled"
    >
      {{ option.label }}
    </option>
  </select>
</template>

<style scoped>
.sp-select {
  width: 100%;
  min-height: var(--sp-control-height);
  padding: 0 var(--sp-space-2);
  color: var(--sp-text);
  font: inherit;
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius-sm);
}
</style>
