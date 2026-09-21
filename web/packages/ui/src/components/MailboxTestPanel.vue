<script setup lang="ts">
import type { components } from '@sendplane/api'
import { computed } from 'vue'

import { useSendplaneOptional } from '../context.js'
import { formatNumber } from '../lib/format.js'

type MailboxTestResult = components['schemas']['MailboxTestResult']

const props = defineProps<{
  result: MailboxTestResult | undefined
}>()

const context = useSendplaneOptional()
const t = (key: string, named?: Record<string, unknown>) => context?.t(key, named) ?? key
const locale = computed(() => context?.locale.value ?? 'en')

const folders = computed(() => Object.entries(props.result?.folders ?? {}))
const missingFolders = computed(() => folders.value.filter(([, folder]) => folder.exists === false))

const hintKey = computed(() => `mailboxTest.stageHint.${props.result?.stage ?? 'config'}`)
</script>

<template>
  <div v-if="result" class="sp-mailbox-test" :class="result.ok ? 'is-ok' : 'is-fail'" role="status">
    <div class="sp-mailbox-test__head">
      <strong>{{ result.ok ? t('mailboxTest.ok') : t('mailboxTest.failed') }}</strong>
      <span class="sp-mono sp-mailbox-test__stage">{{ result.stage }}</span>
    </div>

    <p v-if="!result.ok" class="sp-mailbox-test__hint">{{ t(hintKey) }}</p>

    <p v-if="!result.ok && result.error" class="sp-mailbox-test__error sp-mono">
      {{ result.error }}
    </p>

    <p v-if="!result.ok && result.stage === 'folder' && missingFolders.length" class="sp-mailbox-test__hint">
      {{
        t('mailboxTest.foldersMissing', {
          folders: missingFolders.map(([name]) => name).join(', '),
        })
      }}
    </p>

    <dl class="sp-detail-list">
      <dt>{{ t('mailboxTest.latency') }}</dt>
      <dd>{{ formatNumber(result.latency_ms, locale) }} ms</dd>
    </dl>

    <div v-if="folders.length" class="sp-mailbox-test__folders">
      <h4>{{ t('mailboxTest.folders') }}</h4>
      <ul>
        <li v-for="[name, folder] in folders" :key="name">
          <span class="sp-mono">{{ name }}</span>
          <span v-if="folder.exists">
            — {{ t('mailboxTest.messages', { count: folder.messages ?? 0 }) }}
          </span>
          <span v-else class="sp-mailbox-test__missing">— {{ t('mailboxTest.folderMissing') }}</span>
        </li>
      </ul>
    </div>

    <details v-if="result.server" class="sp-mailbox-test__server">
      <summary>{{ t('mailboxTest.server') }}</summary>
      <p class="sp-mono">{{ result.server }}</p>
    </details>
  </div>
</template>

<style scoped>
.sp-mailbox-test {
  display: flex;
  flex-direction: column;
  gap: var(--sp-space-2);
  padding: var(--sp-space-3);
  border: 1px solid transparent;
  border-radius: var(--sp-radius);
}

.sp-mailbox-test.is-ok {
  color: var(--sp-ok);
  background: var(--sp-ok-soft);
  border-color: var(--sp-ok);
}

.sp-mailbox-test.is-fail {
  color: var(--sp-danger);
  background: var(--sp-danger-soft);
  border-color: var(--sp-danger);
}

.sp-mailbox-test__head {
  display: flex;
  gap: var(--sp-space-2);
  align-items: baseline;
}

.sp-mailbox-test__stage {
  color: var(--sp-text-muted);
}

.sp-mailbox-test__hint {
  margin: 0;
  color: var(--sp-text);
}

.sp-mailbox-test__error {
  margin: 0;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
}

.sp-mailbox-test__missing {
  color: var(--sp-danger);
}

.sp-mailbox-test__folders ul {
  padding-left: var(--sp-space-4);
  margin: 0;
}

.sp-mailbox-test__server summary {
  cursor: pointer;
}
</style>
