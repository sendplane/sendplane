<script setup lang="ts" generic="T">
import { computed } from 'vue'

import { useSendplaneOptional } from '../context.js'
import SpEmptyState from './SpEmptyState.vue'

export interface TableColumn {
  key: string
  label: string
  /** CSS width for the column, e.g. `120px` or `minmax(0, 2fr)`. */
  width?: string
  align?: 'start' | 'end'
  /** Renders the default cell in the monospace face (IDs, codes). */
  mono?: boolean
  /** Hidden below 640px, so a phone keeps the columns that matter. */
  secondary?: boolean
}

const props = withDefaults(
  defineProps<{
    columns: TableColumn[]
    rows: readonly T[]
    rowKey: (row: T, index: number) => string
    caption?: string
    loading?: boolean
    emptyTitle?: string
    emptyDescription?: string
    /** Cursor pagination state; omit the footer by leaving both false. */
    hasNext?: boolean
    hasPrevious?: boolean
    pageNumber?: number
    paginated?: boolean
  }>(),
  { paginated: true },
)

const emit = defineEmits<{ next: []; previous: [] }>()

const context = useSendplaneOptional()
const t = (key: string, named?: Record<string, unknown>) => context?.t(key, named) ?? key

const showFooter = computed(() => props.paginated && (props.hasNext || props.hasPrevious))

/** Default cell rendering: the row's own field under the column key. */
function rawCell(row: T, key: string): unknown {
  return (row as Record<string, unknown>)[key] ?? '—'
}
const showEmpty = computed(() => !props.loading && props.rows.length === 0)
</script>

<template>
  <div class="sp-table-wrap">
    <div class="sp-table-scroll">
      <table class="sp-table">
        <caption v-if="caption" class="sp-visually-hidden">
          {{
            caption
          }}
        </caption>
        <thead>
          <tr>
            <th
              v-for="column in columns"
              :key="column.key"
              scope="col"
              :class="{
                'sp-table__cell--end': column.align === 'end',
                'sp-table__cell--secondary': column.secondary,
              }"
              :style="column.width ? { width: column.width } : undefined"
            >
              {{ column.label }}
            </th>
          </tr>
        </thead>
        <tbody :aria-busy="loading ? 'true' : undefined">
          <tr v-for="(row, index) in rows" :key="rowKey(row, index)">
            <td
              v-for="column in columns"
              :key="column.key"
              :class="{
                'sp-table__cell--end': column.align === 'end',
                'sp-table__cell--secondary': column.secondary,
                'sp-mono': column.mono,
              }"
            >
              <slot :name="`cell-${column.key}`" :row="row" :index="index" :column="column">
                {{ rawCell(row, column.key) }}
              </slot>
            </td>
          </tr>
          <tr v-if="loading && rows.length === 0" class="sp-table__status">
            <td :colspan="columns.length">{{ t('common.loading') }}</td>
          </tr>
        </tbody>
      </table>
    </div>

    <SpEmptyState
      v-if="showEmpty"
      :title="emptyTitle ?? t('empty.nothing')"
      :description="emptyDescription"
    >
      <slot name="empty" />
    </SpEmptyState>

    <nav v-if="showFooter" class="sp-table__pager" :aria-label="t('common.actions')">
      <button
        type="button"
        class="sp-table__pagebtn"
        :disabled="!hasPrevious || loading"
        @click="emit('previous')"
      >
        ← {{ t('common.newer') }}
      </button>
      <span class="sp-table__pageno">{{ t('table.page', { page: pageNumber ?? 1 }) }}</span>
      <button
        type="button"
        class="sp-table__pagebtn"
        :disabled="!hasNext || loading"
        @click="emit('next')"
      >
        {{ t('common.older') }} →
      </button>
    </nav>
  </div>
</template>

<style scoped>
.sp-table-wrap {
  background: var(--sp-surface);
  border: 1px solid var(--sp-border);
  border-radius: var(--sp-radius);
  overflow: hidden;
}

.sp-table-scroll {
  overflow-x: auto;
}

.sp-table {
  width: 100%;
  border-collapse: collapse;
  font-variant-numeric: tabular-nums;
}

.sp-table th,
.sp-table td {
  height: var(--sp-row-height);
  padding: var(--sp-space-1) var(--sp-space-3);
  text-align: start;
  vertical-align: middle;
  border-bottom: 1px solid var(--sp-border);
}

.sp-table th {
  position: sticky;
  top: 0;
  z-index: 1;
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
  font-weight: 600;
  background: var(--sp-surface-alt);
  white-space: nowrap;
}

.sp-table tbody tr:last-child td {
  border-bottom: 0;
}

.sp-table tbody tr:hover td {
  background: var(--sp-surface-hover);
}

.sp-table__cell--end {
  text-align: end;
}

.sp-table__status td {
  color: var(--sp-text-muted);
  text-align: center;
}

.sp-table__pager {
  display: flex;
  gap: var(--sp-space-3);
  align-items: center;
  justify-content: flex-end;
  padding: var(--sp-space-2) var(--sp-space-3);
  border-top: 1px solid var(--sp-border);
}

.sp-table__pageno {
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
}

.sp-table__pagebtn {
  min-height: 26px;
  padding: 0 var(--sp-space-2);
  color: inherit;
  font: inherit;
  font-size: var(--sp-font-size-sm);
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius-sm);
  cursor: pointer;
}

.sp-table__pagebtn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

@media (width <= 640px) {
  .sp-table__cell--secondary {
    display: none;
  }
}
</style>
