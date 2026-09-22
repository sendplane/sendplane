import { useToast, type ToastApi } from './useToast.js'
import { useSendplaneOptional } from '../context.js'
import { isPlatformReadOnly } from '../lib/platform.js'

/**
 * `useToast()` with the platform-resource failures translated.
 *
 * A `403 platform_read_only` is not an operator mistake to be debugged from an
 * error code — it means the row is the operator's configuration — so it gets a
 * sentence instead of `platform_read_only: …`. Everything else falls through to
 * the generic description.
 */
export function useApiToast(): ToastApi {
  const toast = useToast()
  const context = useSendplaneOptional()

  return {
    ...toast,
    fail: (error, fallback) => {
      if (isPlatformReadOnly(error)) {
        const message = context?.t('shared.readOnly')
        // Without a context there is no translation to show, so keep the raw one.
        if (message && message !== 'shared.readOnly') return toast.push(message, 'danger', 0)
      }
      return fallback === undefined ? toast.fail(error) : toast.fail(error, fallback)
    },
  }
}
