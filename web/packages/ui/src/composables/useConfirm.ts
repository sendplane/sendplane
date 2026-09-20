import { inject, provide, ref, type InjectionKey, type Ref } from 'vue'

export interface ConfirmRequest {
  message: string
  title?: string
  confirmLabel?: string
  cancelLabel?: string
  /** Paints the confirm button as destructive. */
  danger?: boolean
}

export interface ConfirmApi {
  /** Resolves true when the operator confirmed. */
  ask: (request: ConfirmRequest | string) => Promise<boolean>
  /** Internal state the dialog host renders. */
  current: Ref<(ConfirmRequest & { resolve: (ok: boolean) => void }) | null>
}

const CONFIRM_KEY: InjectionKey<ConfirmApi> = Symbol('sendplane.confirm')

export function createConfirmApi(): ConfirmApi {
  const current = ref<(ConfirmRequest & { resolve: (ok: boolean) => void }) | null>(null)

  return {
    current,
    ask(request) {
      const normalized = typeof request === 'string' ? { message: request } : request
      return new Promise<boolean>((resolve) => {
        current.value = {
          ...normalized,
          resolve: (ok) => {
            current.value = null
            resolve(ok)
          },
        }
      })
    },
  }
}

export function provideConfirmApi(api: ConfirmApi = createConfirmApi()): ConfirmApi {
  provide(CONFIRM_KEY, api)
  return api
}

/**
 * Falls back to the platform `confirm()` when no dialog host is mounted, so a
 * destructive action is never silently confirmed.
 */
export function useConfirm(): ConfirmApi['ask'] {
  const api = inject(CONFIRM_KEY, null)
  if (api) return api.ask
  return (request) => {
    const message = typeof request === 'string' ? request : request.message
    if (typeof window === 'undefined') return Promise.resolve(false)
    return Promise.resolve(window.confirm(message))
  }
}
