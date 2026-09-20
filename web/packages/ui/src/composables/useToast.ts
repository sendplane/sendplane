import { isSendplaneError } from '@sendplane/api'
import { inject, provide, ref, type InjectionKey, type Ref } from 'vue'

export type ToastKind = 'info' | 'success' | 'warning' | 'danger'

export interface Toast {
  id: number
  kind: ToastKind
  message: string
  /** Milliseconds before it auto-dismisses; 0 keeps it until dismissed. */
  timeout: number
}

export interface ToastApi {
  toasts: Ref<Toast[]>
  push: (message: string, kind?: ToastKind, timeout?: number) => number
  success: (message: string) => number
  /** Turns any thrown value into a readable message. */
  fail: (error: unknown, fallback?: string) => number
  dismiss: (id: number) => void
}

const TOAST_KEY: InjectionKey<ToastApi> = Symbol('sendplane.toast')

export function createToastApi(): ToastApi {
  const toasts = ref<Toast[]>([])
  let nextId = 1

  function dismiss(id: number): void {
    toasts.value = toasts.value.filter((toast) => toast.id !== id)
  }

  function push(message: string, kind: ToastKind = 'info', timeout = 5000): number {
    const id = nextId++
    toasts.value = [...toasts.value, { id, kind, message, timeout }]
    if (timeout > 0 && typeof window !== 'undefined') {
      window.setTimeout(() => dismiss(id), timeout)
    }
    return id
  }

  return {
    toasts,
    push,
    dismiss,
    success: (message) => push(message, 'success'),
    fail: (error, fallback = 'Request failed') =>
      // Errors stay until dismissed: an ops console failure is worth reading.
      push(describeError(error, fallback), 'danger', 0),
  }
}

export function provideToastApi(api: ToastApi = createToastApi()): ToastApi {
  provide(TOAST_KEY, api)
  return api
}

/**
 * Toasts are optional: a host that mounts a single page without the provider
 * still gets working buttons, just without the notifications.
 */
export function useToast(): ToastApi {
  return inject(TOAST_KEY, null) ?? noopToasts
}

export function describeError(error: unknown, fallback = 'Request failed'): string {
  if (isSendplaneError(error)) {
    const detail = error.details[0]
    const where = detail?.field ? `${detail.field}: ` : ''
    const extra = detail?.message ? ` (${where}${detail.message})` : ''
    return `${error.code}: ${error.message}${extra}`
  }
  if (error instanceof Error) return error.message
  return fallback
}

const noopToasts: ToastApi = {
  toasts: ref([]),
  push: () => 0,
  success: () => 0,
  fail: (error) => {
    console.error('[@sendplane/ui]', describeError(error))
    return 0
  },
  dismiss: () => {},
}
