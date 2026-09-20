<script setup lang="ts">
const props = defineProps<{
  legend: string
  options: { value: string; label: string }[]
  modelValue: string[]
}>()

const emit = defineEmits<{ 'update:modelValue': [string[]] }>()

function toggle(value: string) {
  const next = props.modelValue.includes(value)
    ? props.modelValue.filter((entry) => entry !== value)
    : [...props.modelValue, value]
  emit('update:modelValue', next)
}
</script>

<template>
  <!--
    A checkbox group rather than a multi-select: every repeated filter in the
    spec is `explode: true`, so the operator is really ticking independent
    values, and checkboxes are keyboard-navigable without a popup.
  -->
  <fieldset class="sp-filter">
    <legend class="sp-filter__legend">{{ legend }}</legend>
    <div class="sp-filter__chips">
      <label
        v-for="option in options"
        :key="option.value"
        class="sp-filter__chip"
        :class="{ 'is-on': modelValue.includes(option.value) }"
      >
        <input
          type="checkbox"
          class="sp-visually-hidden"
          :checked="modelValue.includes(option.value)"
          @change="toggle(option.value)"
        />
        {{ option.label }}
      </label>
    </div>
  </fieldset>
</template>

<style scoped>
.sp-filter {
  min-width: 0;
  margin: 0;
  padding: 0;
  border: 0;
}

.sp-filter__legend {
  padding: 0;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
  font-weight: 600;
}

.sp-filter__chips {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-1);
  margin-top: var(--sp-space-1);
}

.sp-filter__chip {
  padding: 2px var(--sp-space-2);
  font-size: var(--sp-font-size-sm);
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: 999px;
  cursor: pointer;
}

.sp-filter__chip:has(:focus-visible) {
  outline: 2px solid var(--sp-focus);
  outline-offset: 2px;
}

.sp-filter__chip.is-on {
  color: var(--sp-accent);
  background: var(--sp-accent-soft);
  border-color: var(--sp-accent);
}
</style>
