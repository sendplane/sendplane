<script setup lang="ts">
import { defineAsyncComponent } from 'vue'

/**
 * Placeholder for the block editor.
 *
 * The GrapesJS + grapesjs-mjml wrapper is a spike that has not landed yet
 * (architecture 6.3, roadmap phase 8). The seam is already here, dynamically
 * imported, so replacing the placeholder with the real editor is a one-line
 * change and nothing else pulls GrapesJS into the bundle in the meantime.
 *
 * A host that already has its own block editor can pass it through the default
 * slot instead.
 */
defineProps<{ modelValue: Record<string, unknown> | undefined; mjml: string }>()
defineEmits<{ 'update:modelValue': [Record<string, unknown>]; 'update:mjml': [string] }>()

const Placeholder = defineAsyncComponent(() => import('./BlockEditorPlaceholder.vue'))
</script>

<template>
  <slot>
    <Placeholder />
  </slot>
</template>
