<script setup lang="ts">
import { computed, defineAsyncComponent } from 'vue'

import { makeBlockProject, readBlockProject, type BlockEditorValue } from '../lib/mjml-blocks.js'

/**
 * Seam between the template editor and the block editor.
 *
 * It owns the ADR-0009 storage envelope — `{editor, editor_version, project,
 * mjml}` in `Template.blocks` — so `MjmlBlockEditor` only deals in a project
 * blob plus its export, and a host that swaps in its own editor through the
 * default slot inherits nothing of ours.
 *
 * GrapesJS plus the MJML compiler is by far the heaviest thing in the console,
 * so the editor is dynamically imported: it only reaches the browser when the
 * blocks tab is actually opened.
 */
const props = defineProps<{
  /** Whatever is stored in `Template.blocks`. */
  modelValue: Record<string, unknown> | undefined
  /** The exported MJML, which is what `Template.body` holds and the server compiles. */
  mjml: string
  locale?: string
  i18nKeys?: string[]
}>()

const emit = defineEmits<{
  'update:modelValue': [Record<string, unknown>]
  'update:mjml': [string]
}>()

const MjmlBlockEditor = defineAsyncComponent(() => import('./MjmlBlockEditor.vue'))

const stored = computed(() => readBlockProject(props.modelValue))

const value = computed<BlockEditorValue>(() => ({
  project: stored.value.project,
  // `Template.body` wins over the copy inside the envelope: the operator may
  // have edited the MJML on the code tab since the project was saved.
  mjml: props.mjml || stored.value.mjml,
}))

function onUpdate(next: BlockEditorValue) {
  emit(
    'update:modelValue',
    makeBlockProject(next.project, next.mjml) as unknown as Record<string, unknown>,
  )
  emit('update:mjml', next.mjml)
}
</script>

<template>
  <slot>
    <MjmlBlockEditor
      :model-value="value"
      :locale="locale"
      :i18n-keys="i18nKeys"
      @update:model-value="onUpdate"
    />
  </slot>
</template>
