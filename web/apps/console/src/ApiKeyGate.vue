<script setup lang="ts">
import { SpButton, SpCard, SpField, SpInput } from '@sendplane/ui'
import { ref } from 'vue'

import { baseUrl, setApiKey } from './auth.js'

const entered = ref('')

function submit() {
  if (entered.value.trim()) setApiKey(entered.value)
}
</script>

<template>
  <div class="gate">
    <SpCard title="sendplane console" :subtitle="baseUrl || 'same origin'">
      <form @submit.prevent="submit">
        <SpField
          v-slot="{ id, describedBy }"
          label="API key"
          hint="Sent as X-API-Key. Kept in sessionStorage, so closing the tab signs you out."
          required
        >
          <SpInput
            :id="id"
            v-model="entered"
            type="password"
            autocomplete="off"
            :described-by="describedBy"
          />
        </SpField>
        <div class="gate__actions">
          <SpButton type="submit" variant="primary" :disabled="!entered.trim()">Continue</SpButton>
        </div>
      </form>
    </SpCard>
  </div>
</template>

<style scoped>
.gate {
  display: grid;
  place-items: center;
  min-height: 100vh;
  padding: var(--sp-space-4);
}

.gate > * {
  width: min(420px, 100%);
}

.gate__actions {
  display: flex;
  justify-content: flex-end;
  margin-top: var(--sp-space-4);
}
</style>
