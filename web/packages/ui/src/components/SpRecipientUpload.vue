<script setup lang="ts">
import { recipientsFromFile, supportsRequestStreams, type RecipientLine } from '@sendplane/api'
import { computed, ref } from 'vue'

import { useToast } from '../composables/useToast.js'
import { useSendplane } from '../context.js'
import { formatNumber } from '../lib/format.js'
import SpButton from './SpButton.vue'
import SpCard from './SpCard.vue'
import SpField from './SpField.vue'
import SpInput from './SpInput.vue'

const props = defineProps<{ campaignId: string; disabled?: boolean }>()
const emit = defineEmits<{ ingested: [] }>()

const { client, t, locale } = useSendplane()
const toast = useToast()

const file = ref<File | null>(null)
const chunkSize = ref(20000)
const keyPrefix = ref('')
const uploading = ref(false)
const sentLines = ref(0)
const totals = ref({ accepted: 0, duplicates: 0, invalid: 0, total: 0 })
const done = ref(false)

const streaming = computed(() => supportsRequestStreams())

function onFile(event: Event) {
  const input = event.target as HTMLInputElement
  file.value = input.files?.[0] ?? null
  done.value = false
  sentLines.value = 0
  totals.value = { accepted: 0, duplicates: 0, invalid: 0, total: 0 }
  if (file.value && !keyPrefix.value) {
    // A stable default so re-running after a network blip replays the chunks
    // rather than double-inserting them.
    keyPrefix.value = `${file.value.name}-${file.value.size}`
  }
}

/**
 * Uploads the file one chunk at a time.
 *
 * Only `chunkSize` recipients are held in memory: the file is read as a stream,
 * each chunk goes out as its own streamed NDJSON request, and each chunk
 * carries its own `Idempotency-Key` so a retry of a partial upload resumes
 * instead of duplicating (architecture 7.2).
 */
async function upload() {
  if (!file.value) return
  uploading.value = true
  done.value = false
  sentLines.value = 0
  totals.value = { accepted: 0, duplicates: 0, invalid: 0, total: 0 }

  try {
    const source = recipientsFromFile(file.value)[Symbol.asyncIterator]()
    const size = Math.max(1, Number(chunkSize.value) || 1)
    let chunkNo = 0
    let batch: RecipientLine[] = []
    let exhausted = false

    while (!exhausted) {
      const next = await source.next()
      if (next.done) exhausted = true
      else batch.push(next.value)

      if (batch.length >= size || (exhausted && batch.length > 0)) {
        const result = await client.ingestRecipients(props.campaignId, batch, {
          idempotencyKey: `${keyPrefix.value || props.campaignId}-chunk-${chunkNo}`,
        })
        totals.value = {
          accepted: totals.value.accepted + (result.accepted ?? 0),
          duplicates: totals.value.duplicates + (result.duplicates ?? 0),
          invalid: totals.value.invalid + (result.invalid ?? 0),
          // `total` is the campaign's running recipient count, not a chunk size,
          // so the last chunk's value is the answer rather than a sum.
          total: result.total ?? totals.value.total,
        }
        sentLines.value += batch.length
        chunkNo++
        batch = []
      }
    }

    done.value = true
    emit('ingested')
  } catch (error) {
    toast.fail(error)
  } finally {
    uploading.value = false
  }
}
</script>

<template>
  <SpCard :title="t('campaign.upload')" :subtitle="t('campaign.uploadHint')">
    <div class="sp-form-grid">
      <SpField v-slot="{ id }" :label="t('campaign.uploadFile')">
        <input
          :id="id"
          class="sp-upload__file"
          type="file"
          accept=".csv,.ndjson,.jsonl,.json,text/csv,application/x-ndjson"
          :disabled="disabled || uploading"
          @change="onFile"
        />
      </SpField>
      <SpField v-slot="{ id }" :label="t('campaign.chunkSize')">
        <SpInput :id="id" v-model="chunkSize" type="number" :min="1" :disabled="uploading" />
      </SpField>
      <SpField v-slot="{ id }" :label="t('campaign.idempotencyKey')">
        <SpInput :id="id" v-model="keyPrefix" :disabled="uploading" />
      </SpField>
    </div>

    <p v-if="!streaming" class="sp-note sp-note--warn sp-page__block">
      {{ t('campaign.notStreaming') }}
    </p>

    <p v-if="uploading" class="sp-note" role="status">
      {{ t('campaign.uploading') }} — {{ formatNumber(sentLines, locale) }}
    </p>

    <p v-else-if="done" class="sp-note" role="status">
      {{
        t('campaign.uploadDone', {
          accepted: formatNumber(totals.accepted, locale),
          duplicates: formatNumber(totals.duplicates, locale),
          invalid: formatNumber(totals.invalid, locale),
          total: formatNumber(totals.total, locale),
        })
      }}
    </p>

    <template #footer>
      <div class="sp-actions-row sp-upload__foot">
        <SpButton
          variant="primary"
          :loading="uploading"
          :disabled="!file || disabled"
          @click="upload"
        >
          {{ t('campaign.startUpload') }}
        </SpButton>
      </div>
    </template>
  </SpCard>
</template>

<style scoped>
.sp-upload__file {
  width: 100%;
  font: inherit;
}

.sp-upload__foot {
  justify-content: flex-end;
}
</style>
