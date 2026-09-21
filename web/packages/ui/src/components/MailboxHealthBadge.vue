<script setup lang="ts">
import type { components } from '@sendplane/api'
import { computed } from 'vue'

import { useSendplaneOptional } from '../context.js'
import SpStatusBadge from './SpStatusBadge.vue'

type MailboxHealth = components['schemas']['MailboxHealth']

const props = defineProps<{
  health: MailboxHealth | undefined
}>()

const context = useSendplaneOptional()
const t = (key: string, named?: Record<string, unknown>) => context?.t(key, named) ?? key

// A tooltip that reads naturally whether or not there is a reason yet.
const title = computed(() => {
  if (!props.health?.reason) return undefined
  return props.health.stage ? `${props.health.stage}: ${props.health.reason}` : props.health.reason
})
</script>

<template>
  <span class="sp-mailbox-health">
    <SpStatusBadge kind="mailbox" :value="health?.status" :title="title" />
    <span v-if="health?.status === 'error' && health?.stage" class="sp-mailbox-health__stage sp-mono">
      {{ health.stage }}
    </span>
    <span v-if="health?.status === 'error' && health?.reason" class="sp-mailbox-health__reason">
      {{ health.reason }}
    </span>
    <span v-if="health?.status === 'error' && health?.consecutive_failures" class="sp-mailbox-health__failures">
      {{ t('mailboxTest.healthFailures') }}: {{ health.consecutive_failures }}
    </span>
  </span>
</template>

<style scoped>
.sp-mailbox-health {
  display: inline-flex;
  flex-wrap: wrap;
  gap: var(--sp-space-1);
  align-items: center;
}

.sp-mailbox-health__stage {
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}

.sp-mailbox-health__reason,
.sp-mailbox-health__failures {
  color: var(--sp-danger);
  font-size: var(--sp-font-size-sm);
}
</style>
