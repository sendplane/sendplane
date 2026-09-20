<script setup lang="ts">
import { computed } from 'vue'

import { describeError } from '../composables/useToast.js'
import { useSendplaneOptional } from '../context.js'
import SpButton from './SpButton.vue'

const props = defineProps<{ error: unknown; onRetry?: () => void }>()

const context = useSendplaneOptional()
const t = (key: string) => context?.t(key) ?? key
const message = computed(() => describeError(props.error, t('error.title')))
</script>

<template>
  <div v-if="error" class="sp-error" role="alert">
    <div class="sp-error__text">
      <strong>{{ t('error.title') }}</strong>
      <span>{{ message }}</span>
    </div>
    <SpButton v-if="onRetry" size="sm" @click="onRetry">{{ t('common.retry') }}</SpButton>
  </div>
</template>

<style scoped>
.sp-error {
  display: flex;
  gap: var(--sp-space-3);
  align-items: center;
  justify-content: space-between;
  padding: var(--sp-space-2) var(--sp-space-3);
  color: var(--sp-danger);
  background: var(--sp-danger-soft);
  border: 1px solid var(--sp-danger);
  border-radius: var(--sp-radius);
}

.sp-error__text {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
  overflow-wrap: anywhere;
}
</style>
