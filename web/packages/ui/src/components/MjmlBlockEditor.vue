<script setup lang="ts">
import type { Component as GjsComponent, Editor } from 'grapesjs'
import grapesjs from 'grapesjs'
// Injected at mount instead of imported as a stylesheet: in a library build a
// plain CSS import is hoisted into the package's single stylesheet, which every
// host would load whether or not it ever opens the block editor. As a string it
// stays inside this lazy chunk.
import grapesCss from 'grapesjs/dist/css/grapes.min.css?inline'
import mjmlPlugin from 'grapesjs-mjml'
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, useId, watch } from 'vue'

import { useSendplaneOptional } from '../context.js'
import {
  blockDefinitions,
  DEFAULT_MJML,
  extractI18nKeys,
  finishExport,
  i18nTag,
  isMjmlDocument,
  replaceI18nKey,
  SP_I18N_CLASS,
  type BlockEditorValue,
} from '../lib/mjml-blocks.js'

const props = withDefaults(
  defineProps<{
    modelValue: BlockEditorValue
    /** Locale the operator is authoring for; only labels the i18n panel. */
    locale?: string
    /** Keys the bundle already defines, offered by the i18n block. */
    i18nKeys?: string[]
    /** How long editing has to settle before the parent is told. */
    debounceMs?: number
    height?: string
  }>(),
  { locale: '', i18nKeys: () => [], debounceMs: 400, height: '560px' },
)

const emit = defineEmits<{ 'update:modelValue': [BlockEditorValue] }>()

const context = useSendplaneOptional()
const t = (key: string) => context?.t(key) ?? key
const uid = useId()

const host = ref<HTMLElement | null>(null)
// `shallowRef`: the GrapesJS editor is a large mutable object with its own
// change tracking, and a Vue proxy around it breaks Backbone's identity checks.
const editor = shallowRef<Editor | null>(null)
const failed = ref('')
const ready = ref(false)
const showCode = ref(false)
const exported = ref(props.modelValue.mjml ?? '')
const device = ref('')
const canUndo = ref(false)
const canRedo = ref(false)
const selectedKey = ref<string | null>(null)
const keyDraft = ref('')

/** Set while the editor is being loaded from props, to mute the change hook. */
let loading = false
let timer: ReturnType<typeof setTimeout> | undefined
/** The MJML this component last emitted, so its own echo is not re-imported. */
let lastEmitted = ''

const devices = computed(() =>
  (editor.value?.Devices.getDevices() ?? []).map((d) => ({
    id: String(d.id),
    label: String(d.get('name') ?? d.id),
  })),
)

const usedKeys = computed(() => extractI18nKeys(exported.value))
const missingKeys = computed(() => {
  const known = new Set(props.i18nKeys)
  return usedKeys.value.filter((key) => !known.has(key))
})
const keyOptions = computed(() => {
  const all = new Set([...props.i18nKeys, ...usedKeys.value])
  return [...all].sort()
})

// --- boot -------------------------------------------------------------------

/**
 * Registers the sendplane-specific block palette and the component type that
 * lets the i18n block be recognised again after a reload.
 *
 * The marker is a `css-class`, not a `data-*` attribute: MJML's strict
 * validator (which `internal/render.compileMJML` uses) rejects unknown
 * attributes on `mj-*` elements, so a `data-sp-i18n` marker would make the
 * template unpublishable.
 */
function sendplanePlugin(ed: Editor) {
  ed.DomComponents.addType('sp-i18n-text', {
    extend: 'mj-text',
    isComponent: (el: HTMLElement) =>
      el.tagName === 'MJ-TEXT' &&
      (el.getAttribute?.('css-class') ?? '').split(/\s+/).includes(SP_I18N_CLASS),
    model: { defaults: { name: t('blockEditor.blocks.i18nText') } },
  })

  for (const block of blockDefinitions({ t })) {
    ed.Blocks.add(block.id, {
      label: block.label,
      category: block.category,
      content: block.content,
      select: true,
    })
  }
}

function initialSource(): string {
  const { mjml } = props.modelValue
  return mjml && isMjmlDocument(mjml) ? mjml : DEFAULT_MJML
}

const STYLE_ID = 'sp-grapesjs-css'
let styleAttached = false

/**
 * Adds the GrapesJS stylesheet once per document and reference-counts it, so a
 * second editor on the same screen does not remove the styles of the first when
 * it unmounts.
 */
function attachStyles() {
  if (typeof document === 'undefined' || styleAttached) return
  let el = document.getElementById(STYLE_ID) as HTMLStyleElement | null
  if (!el) {
    el = document.createElement('style')
    el.id = STYLE_ID
    el.textContent = grapesCss
    document.head.appendChild(el)
  }
  el.dataset.spRefs = String(Number(el.dataset.spRefs ?? '0') + 1)
  styleAttached = true
}

function detachStyles() {
  if (typeof document === 'undefined' || !styleAttached) return
  styleAttached = false
  const el = document.getElementById(STYLE_ID) as HTMLStyleElement | null
  if (!el) return
  const refs = Number(el.dataset.spRefs ?? '1') - 1
  if (refs > 0) el.dataset.spRefs = String(refs)
  else el.remove()
}

onMounted(() => {
  if (!host.value) return
  try {
    attachStyles()
    loading = true
    const instance = grapesjs.init({
      container: host.value,
      height: props.height,
      width: 'auto',
      fromElement: false,
      storageManager: false,
      undoManager: true,
      // The console owns the outer chrome; GrapesJS keeps only the right-hand
      // views panel (blocks, styles, traits, layers). Its own top bar is hidden
      // in this component's stylesheet in favour of the toolbar below.
      plugins: [mjmlPlugin, sendplanePlugin],
      pluginsOpts: { [mjmlPlugin as unknown as string]: {} },
      components: initialSource(),
    })
    editor.value = instance

    instance.onReady(() => {
      const stored = props.modelValue.project
      if (stored) {
        try {
          instance.loadProjectData(stored as Parameters<Editor['loadProjectData']>[0])
        } catch {
          // A project from another editor version may not load; the MJML the
          // component already imported is the authoritative fallback.
          failed.value = t('blockEditor.projectFailed')
        }
      }
      device.value = String(instance.getDevice() ?? '')
      exported.value = finishExport(String(instance.runCommand('mjml-code') ?? ''))
      lastEmitted = exported.value
      loading = false
      ready.value = true

      instance.on('update', scheduleEmit)
      instance.on('component:selected', syncSelection)
      instance.on('component:deselected', syncSelection)
      instance.on('undo', refreshUndo)
      instance.on('redo', refreshUndo)
      refreshUndo()
      // GrapesJS opens the style manager first; for a mail template the block
      // palette is the panel an operator actually reaches for.
      instance.runCommand('open-blocks')
    })
  } catch (error) {
    failed.value = error instanceof Error ? error.message : String(error)
  }
})

onBeforeUnmount(() => {
  if (timer) clearTimeout(timer)
  editor.value?.destroy()
  editor.value = null
  detachStyles()
})

// --- change propagation -----------------------------------------------------

function scheduleEmit() {
  if (loading) return
  refreshUndo()
  if (timer) clearTimeout(timer)
  timer = setTimeout(emitNow, props.debounceMs)
}

function emitNow() {
  const instance = editor.value
  if (!instance) return
  exported.value = finishExport(String(instance.runCommand('mjml-code') ?? ''))
  lastEmitted = exported.value
  refreshUndo()
  emit('update:modelValue', { project: instance.getProjectData(), mjml: exported.value })
}

function refreshUndo() {
  const um = editor.value?.UndoManager
  canUndo.value = um?.hasUndo() ?? false
  canRedo.value = um?.hasRedo() ?? false
}

/**
 * Re-imports the document when the parent hands over MJML this component did
 * not produce — a reload from the server, or an operator who edited the MJML on
 * the code tab and came back.
 */
watch(
  () => props.modelValue.mjml,
  (next) => {
    const instance = editor.value
    if (!instance || !ready.value || !next || next === lastEmitted) return
    loading = true
    instance.setComponents(next)
    exported.value = finishExport(String(instance.runCommand('mjml-code') ?? ''))
    lastEmitted = exported.value
    loading = false
  },
)

// --- toolbar ----------------------------------------------------------------

function undo() {
  editor.value?.UndoManager.undo()
  scheduleEmit()
}

function redo() {
  editor.value?.UndoManager.redo()
  scheduleEmit()
}

function setDevice(id: string) {
  device.value = id
  editor.value?.setDevice(id)
}

// --- i18n block -------------------------------------------------------------

function selectedI18n(): GjsComponent | null {
  const selected = editor.value?.getSelected()
  if (!selected) return null
  return selected.get('type') === 'sp-i18n-text' ? selected : null
}

function syncSelection() {
  const component = selectedI18n()
  if (!component) {
    selectedKey.value = null
    return
  }
  const [key] = extractI18nKeys(component.toHTML())
  selectedKey.value = key ?? ''
  keyDraft.value = key ?? ''
}

function applyKey() {
  const component = selectedI18n()
  if (!component) return
  const key = keyDraft.value.trim()
  if (!key) return
  component.components(replaceI18nKey(component.toHTML(), key))
  selectedKey.value = key
  scheduleEmit()
}

/**
 * The GrapesJS instance is exposed so a host can extend the palette or drive
 * the canvas from its own chrome; `exportMjml` is the same string the component
 * emits, already Liquid-corrected.
 */
defineExpose({ editor, exportMjml: () => exported.value, i18nTag })
</script>

<template>
  <div class="sp-blocks">
    <p v-if="failed" class="sp-note sp-note--warn sp-blocks__notice">
      {{ t('blockEditor.failed') }} — {{ failed }}
    </p>

    <div class="sp-blocks__toolbar" role="toolbar" :aria-label="t('blockEditor.toolbar')">
      <button
        type="button"
        class="sp-blocks__btn"
        :disabled="!canUndo"
        :title="t('blockEditor.undo')"
        @click="undo"
      >
        {{ t('blockEditor.undo') }}
      </button>
      <button
        type="button"
        class="sp-blocks__btn"
        :disabled="!canRedo"
        :title="t('blockEditor.redo')"
        @click="redo"
      >
        {{ t('blockEditor.redo') }}
      </button>

      <span class="sp-blocks__sep" aria-hidden="true"></span>

      <label class="sp-blocks__label" :for="`${uid}-device`">{{ t('blockEditor.device') }}</label>
      <select
        :id="`${uid}-device`"
        class="sp-blocks__select"
        :value="device"
        @change="setDevice(($event.target as HTMLSelectElement).value)"
      >
        <option v-for="item in devices" :key="item.id" :value="item.id">{{ item.label }}</option>
      </select>

      <span class="sp-blocks__spacer"></span>

      <button
        type="button"
        class="sp-blocks__btn"
        :aria-pressed="showCode"
        @click="showCode = !showCode"
      >
        {{ t('blockEditor.viewMjml') }}
      </button>
    </div>

    <div v-if="selectedKey !== null" class="sp-blocks__i18n">
      <label class="sp-blocks__label" :for="`${uid}-key`">
        {{ t('blockEditor.i18nKey') }}
        <span v-if="locale" class="sp-muted">({{ locale }})</span>
      </label>
      <input
        :id="`${uid}-key`"
        v-model="keyDraft"
        class="sp-blocks__input"
        :list="`${uid}-keys`"
        spellcheck="false"
        @keydown.enter.prevent="applyKey"
      />
      <datalist :id="`${uid}-keys`">
        <option v-for="key in keyOptions" :key="key" :value="key" />
      </datalist>
      <button type="button" class="sp-blocks__btn" @click="applyKey">
        {{ t('common.apply') }}
      </button>
      <span v-if="keyDraft && !i18nKeys.includes(keyDraft)" class="sp-blocks__warn">
        {{ t('blockEditor.keyUnknown') }}
      </span>
    </div>

    <p v-if="missingKeys.length" class="sp-note sp-note--warn sp-blocks__notice">
      {{ t('blockEditor.missingKeys') }}: {{ missingKeys.join(', ') }}
    </p>

    <div ref="host" class="sp-blocks__canvas"></div>

    <pre v-if="showCode" class="sp-blocks__code" :aria-label="t('blockEditor.viewMjml')">{{
      exported
    }}</pre>
  </div>
</template>

<style scoped>
.sp-blocks {
  display: flex;
  flex-direction: column;
  gap: var(--sp-space-2);
}

.sp-blocks__toolbar,
.sp-blocks__i18n {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-2);
  align-items: center;
  padding: var(--sp-space-2);
  background: var(--sp-surface-alt);
  border: 1px solid var(--sp-border);
  border-radius: var(--sp-radius);
}

.sp-blocks__btn {
  padding: var(--sp-space-1) var(--sp-space-3);
  color: var(--sp-text);
  font: inherit;
  font-size: var(--sp-font-size-sm);
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius);
  cursor: pointer;
}

.sp-blocks__btn:disabled {
  color: var(--sp-text-muted);
  cursor: not-allowed;
  opacity: 0.6;
}

.sp-blocks__btn:focus-visible,
.sp-blocks__select:focus-visible,
.sp-blocks__input:focus-visible {
  outline: 2px solid var(--sp-focus);
  outline-offset: 1px;
}

.sp-blocks__label {
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}

.sp-blocks__select,
.sp-blocks__input {
  padding: var(--sp-space-1) var(--sp-space-2);
  color: var(--sp-text);
  font: inherit;
  font-size: var(--sp-font-size-sm);
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius);
}

.sp-blocks__input {
  min-width: 22ch;
  font-family: var(--sp-font-mono);
}

.sp-blocks__sep {
  width: 1px;
  height: 18px;
  background: var(--sp-border-strong);
}

.sp-blocks__spacer {
  flex: 1;
}

.sp-blocks__warn {
  color: var(--sp-warn);
  font-size: var(--sp-font-size-sm);
}

.sp-blocks__notice {
  margin: 0;
}

.sp-blocks__canvas {
  overflow: hidden;
  border: 1px solid var(--sp-border);
  border-radius: var(--sp-radius);
}

.sp-blocks__code {
  max-height: 260px;
  margin: 0;
  padding: var(--sp-space-3);
  overflow: auto;
  color: var(--sp-text);
  font-family: var(--sp-font-mono);
  font-size: var(--sp-font-size-sm);
  white-space: pre-wrap;
  background: var(--sp-surface-alt);
  border: 1px solid var(--sp-border);
  border-radius: var(--sp-radius);
}

/*
 * GrapesJS builds its own DOM imperatively, so scoped selectors never reach it;
 * `:deep()` from the component root is how the console restyles it. Only the
 * frame around the editor is touched — the canvas renders the operator's mail
 * and must keep MJML's own styling.
 */
.sp-blocks :deep(.gjs-pn-options),
.sp-blocks :deep(.gjs-pn-devices-c) {
  display: none;
}

.sp-blocks :deep(.gjs-one-bg) {
  background-color: var(--sp-surface-alt);
}

.sp-blocks :deep(.gjs-two-color) {
  color: var(--sp-text);
}

.sp-blocks :deep(.gjs-four-color),
.sp-blocks :deep(.gjs-four-color-h:hover) {
  color: var(--sp-accent);
}

.sp-blocks :deep(.gjs-block) {
  border-radius: var(--sp-radius);
}
</style>
