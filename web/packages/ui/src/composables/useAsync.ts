import { isSendplaneError, type SendplaneError } from '@sendplane/api'
import {
  onScopeDispose,
  ref,
  shallowRef,
  watch,
  type Ref,
  type ShallowRef,
  type WatchSource,
} from 'vue'

export interface UseAsyncOptions {
  /** Re-runs the loader whenever any of these change. */
  watch?: WatchSource | WatchSource[]
  /** Skip the initial run; call `reload()` when ready. */
  immediate?: boolean
}

export interface UseAsyncResult<T> {
  data: ShallowRef<T | undefined>
  error: ShallowRef<SendplaneError | Error | undefined>
  loading: Ref<boolean>
  /** True until the first successful or failed run finishes. */
  pending: Ref<boolean>
  reload: () => Promise<void>
}

/**
 * Runs an API call and tracks its state, cancelling the in-flight request when
 * inputs change or the component goes away. Out-of-order responses are dropped,
 * so a fast filter change never renders the slower earlier result.
 */
export function useAsync<T>(
  loader: (signal: AbortSignal) => Promise<T>,
  options: UseAsyncOptions = {},
): UseAsyncResult<T> {
  const data = shallowRef<T>()
  const error = shallowRef<SendplaneError | Error>()
  const loading = ref(false)
  const pending = ref(true)

  let controller: AbortController | undefined
  let generation = 0

  async function reload(): Promise<void> {
    controller?.abort()
    controller = new AbortController()
    const mine = ++generation
    loading.value = true
    try {
      const result = await loader(controller.signal)
      if (mine !== generation) return
      data.value = result
      error.value = undefined
    } catch (caught) {
      if (mine !== generation) return
      if (isAbort(caught)) return
      error.value = isSendplaneError(caught)
        ? caught
        : caught instanceof Error
          ? caught
          : new Error(String(caught))
    } finally {
      if (mine === generation) {
        loading.value = false
        pending.value = false
      }
    }
  }

  if (options.watch) {
    watch(options.watch as WatchSource, () => void reload(), {
      immediate: options.immediate !== false,
    })
  } else if (options.immediate !== false) {
    void reload()
  }

  onScopeDispose(() => controller?.abort())

  return { data, error, loading, pending, reload }
}

function isAbort(error: unknown): boolean {
  if (isSendplaneError(error)) return error.code === 'network_error' && /abort/i.test(error.message)
  return error instanceof Error && error.name === 'AbortError'
}
