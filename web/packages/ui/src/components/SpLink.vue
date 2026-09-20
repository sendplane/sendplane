<script setup lang="ts">
import { computed } from 'vue'

import { useSendplane, type NavigateTarget } from '../context.js'

const props = defineProps<{ to: NavigateTarget }>()

const { navigate, href } = useSendplane()

// A real anchor, so middle-click and "open in new tab" work; the click handler
// hands control to the host router for same-tab navigation.
const url = computed(() => {
  try {
    return href(props.to)
  } catch {
    return '#'
  }
})

function onClick(event: MouseEvent) {
  if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey)
    return
  if (event.button !== 0) return
  event.preventDefault()
  navigate(props.to)
}
</script>

<template>
  <a class="sp-link" :href="url" @click="onClick"><slot /></a>
</template>

<style scoped>
.sp-link {
  color: var(--sp-accent);
  text-decoration: none;
}

.sp-link:hover {
  text-decoration: underline;
}
</style>
