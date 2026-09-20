import type { SendplaneError } from '@sendplane/api'
import {
  computed,
  ref,
  watch,
  type ComputedRef,
  type Ref,
  type ShallowRef,
  type WatchSource,
} from 'vue'

import { useAsync } from './useAsync.js'

export interface CursorPage<T> {
  items: T[]
  next_cursor?: string
}

export interface UseCursorListOptions {
  limit?: number
  /** Re-runs from the first page whenever any of these change. */
  watch?: WatchSource | WatchSource[]
}

export interface UseCursorList<T> {
  items: ComputedRef<T[]>
  data: ShallowRef<CursorPage<T> | undefined>
  error: ShallowRef<SendplaneError | Error | undefined>
  loading: Ref<boolean>
  pending: Ref<boolean>
  limit: Ref<number>
  /** 1-based, for display only: cursor pagination has no page count. */
  pageNumber: ComputedRef<number>
  hasNext: ComputedRef<boolean>
  hasPrevious: ComputedRef<boolean>
  next: () => void
  previous: () => void
  /** Reloads from the first page, e.g. after a mutation. */
  reset: () => void
  reload: () => Promise<void>
}

/**
 * Drives the `{ limit, cursor } -> { items, next_cursor }` pagination every list
 * endpoint uses. Cursors are opaque and one-directional, so going back is a
 * replay of the cursors already visited rather than an offset calculation.
 */
export function useCursorList<T>(
  fetchPage: (
    params: { limit: number; cursor?: string },
    signal: AbortSignal,
  ) => Promise<CursorPage<T>>,
  options: UseCursorListOptions = {},
): UseCursorList<T> {
  const limit = ref(options.limit ?? 50)
  // `undefined` is the first page; each entry is the cursor that opened a page.
  const stack = ref<(string | undefined)[]>([undefined])

  const { data, error, loading, pending, reload } = useAsync(
    (signal) => {
      const cursor = stack.value[stack.value.length - 1]
      return fetchPage(
        cursor === undefined ? { limit: limit.value } : { limit: limit.value, cursor },
        signal,
      )
    },
    { watch: [() => stack.value.length, limit, ...toArray(options.watch)] },
  )

  if (options.watch) {
    watch(options.watch as WatchSource, () => {
      // A filter change invalidates every cursor held so far.
      if (stack.value.length > 1) stack.value = [undefined]
    })
  }

  return {
    data,
    error,
    loading,
    pending,
    limit,
    items: computed(() => data.value?.items ?? []),
    pageNumber: computed(() => stack.value.length),
    hasNext: computed(() => Boolean(data.value?.next_cursor)),
    hasPrevious: computed(() => stack.value.length > 1),
    next: () => {
      const cursor = data.value?.next_cursor
      if (cursor) stack.value = [...stack.value, cursor]
    },
    previous: () => {
      if (stack.value.length > 1) stack.value = stack.value.slice(0, -1)
    },
    reset: () => {
      if (stack.value.length > 1) stack.value = [undefined]
      else void reload()
    },
    reload,
  }
}

function toArray(source?: WatchSource | WatchSource[]): WatchSource[] {
  if (!source) return []
  return Array.isArray(source) ? source : [source]
}
