<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'

import SpTextarea from './SpTextarea.vue'

const props = withDefaults(
  defineProps<{
    modelValue: string
    /** Picks the CodeMirror language: MJML is XML-shaped, so it reuses `xml`. */
    language?: 'html' | 'mjml' | 'text'
    readonly?: boolean
    height?: string
    ariaLabel?: string
  }>(),
  { language: 'html', height: '460px' },
)

const emit = defineEmits<{ 'update:modelValue': [string] }>()

const host = ref<HTMLElement | null>(null)
// `shallowRef`: the EditorView is a large mutable object Vue must not proxy.
const view = shallowRef<{
  destroy: () => void
  state: unknown
  dispatch: (tr: unknown) => void
} | null>(null)
const failed = ref(false)

/**
 * CodeMirror is several hundred kilobytes, and most console screens never open
 * an editor, so it is dynamically imported: the host's bundler puts it in its
 * own chunk that only loads when this component mounts.
 */
async function mountEditor() {
  if (!host.value) return
  try {
    const [{ EditorState, Compartment }, viewMod, commands] = await Promise.all([
      import('@codemirror/state'),
      import('@codemirror/view'),
      import('@codemirror/commands'),
    ])
    const { EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter } =
      viewMod

    const language = new Compartment()
    const languageExtension = await loadLanguage(props.language)

    const editor = new EditorView({
      parent: host.value,
      state: EditorState.create({
        doc: props.modelValue,
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          highlightActiveLine(),
          commands.history(),
          keymap.of([...commands.defaultKeymap, ...commands.historyKeymap, commands.indentWithTab]),
          language.of(languageExtension),
          EditorView.lineWrapping,
          EditorState.readOnly.of(props.readonly === true),
          EditorView.editable.of(props.readonly !== true),
          EditorView.contentAttributes.of({
            'aria-label': props.ariaLabel ?? 'code editor',
          }),
          EditorView.updateListener.of((update) => {
            if (update.docChanged) emit('update:modelValue', update.state.doc.toString())
          }),
          EditorView.theme({
            '&': { fontSize: 'var(--sp-font-size-sm)', backgroundColor: 'var(--sp-surface)' },
            '.cm-content': { fontFamily: 'var(--sp-font-mono)', color: 'var(--sp-text)' },
            '.cm-gutters': {
              backgroundColor: 'var(--sp-surface-alt)',
              color: 'var(--sp-text-muted)',
              border: 'none',
            },
            '.cm-activeLine': { backgroundColor: 'var(--sp-surface-hover)' },
            '.cm-activeLineGutter': { backgroundColor: 'var(--sp-surface-hover)' },
            '&.cm-focused': { outline: '2px solid var(--sp-focus)' },
          }),
        ],
      }),
    })
    view.value = editor as unknown as typeof view.value
  } catch (error) {
    // A bundler that could not resolve the optional editor must not take the
    // whole screen down: fall back to a textarea.
    console.warn('[@sendplane/ui] code editor unavailable, falling back to a textarea', error)
    failed.value = true
  }
}

async function loadLanguage(language: 'html' | 'mjml' | 'text') {
  if (language === 'html') return (await import('@codemirror/lang-html')).html()
  if (language === 'mjml') return (await import('@codemirror/lang-xml')).xml()
  return []
}

onMounted(() => void mountEditor())
onBeforeUnmount(() => view.value?.destroy())

// External edits (switching templates, loading a fetch result) replace the doc.
watch(
  () => props.modelValue,
  (next) => {
    const editor = view.value as {
      state: { doc: { toString: () => string; length: number } }
      dispatch: (t: unknown) => void
    } | null
    if (!editor) return
    if (editor.state.doc.toString() === next) return
    editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: next } })
  },
)

defineExpose({ failed })
</script>

<template>
  <div class="sp-code" :style="{ '--sp-code-height': height }">
    <SpTextarea
      v-if="failed"
      :model-value="modelValue"
      mono
      :rows="18"
      :disabled="readonly"
      @update:model-value="emit('update:modelValue', $event)"
    />
    <div v-else ref="host" class="sp-code__host" />
  </div>
</template>

<style scoped>
.sp-code__host {
  height: var(--sp-code-height, 460px);
  overflow: auto;
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius-sm);
}

.sp-code__host :deep(.cm-editor) {
  height: 100%;
}
</style>
