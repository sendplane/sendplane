<script setup lang="ts">
import { createClient, isSendplaneError, type Whoami } from '@sendplane/api'
import { navRoutes, SendplaneProvider, type NavigateTarget } from '@sendplane/ui'
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'

import ApiKeyGate from './ApiKeyGate.vue'
import {
  apiKey,
  authHeaders,
  baseUrl,
  clearApiKey,
  locale,
  ownTenantId,
  rememberOwnTenant,
  setLocale,
  setTenant,
  setTheme,
  SYSTEM_TENANT_ID,
  tenantOverride,
  theme,
  type ThemeChoice,
} from './auth.js'

const router = useRouter()

/**
 * One client for the whole app. `getAuthHeaders` is read per request, so
 * replacing the key takes effect without rebuilding anything, and a 401 clears
 * it so the gate comes back instead of the console silently failing.
 */
const client = createClient({
  baseUrl,
  getAuthHeaders: authHeaders,
  fetch: async (input, init) => {
    const response = await fetch(input as RequestInfo, init)
    if (response.status === 401) clearApiKey()
    return response
  },
})

/**
 * `GET /whoami` is the first call after the gate: it needs authentication and
 * no permission, which is exactly what a console needs to render its own chrome
 * before it knows what the caller may do. It is reloaded after a tenant switch,
 * because the answer is per-request — the resolver decides, not the console.
 */
const whoami = ref<Whoami | undefined>()

async function loadWhoami() {
  if (!apiKey.value) {
    whoami.value = undefined
    return
  }
  try {
    const me = await client.get('/api/v1/whoami')
    whoami.value = me
    rememberOwnTenant(me.tenant_id)
  } catch (error) {
    // A console that cannot identify itself still renders: every page falls
    // back to the tenant view, which is the safe one.
    whoami.value = undefined
    if (!isSendplaneError(error)) console.error(error)
  }
}

watch(apiKey, () => void loadWhoami(), { immediate: true })

const systemView = computed(() => whoami.value?.system_tenant === true)
const canSwitchTenant = computed(() => whoami.value?.can_switch_tenant === true)

/**
 * The switcher's value is the header the console sends, not the tenant it is
 * currently in: an empty header means "my own tenant, whichever the resolver
 * says that is".
 */
const tenantChoice = computed(() =>
  tenantOverride.value === SYSTEM_TENANT_ID ? SYSTEM_TENANT_ID : '',
)

const ownTenantLabel = computed(() => ownTenantId.value || whoami.value?.tenant_id || '')

async function switchTenant(value: string) {
  if (value === tenantChoice.value) return
  setTenant(value === SYSTEM_TENANT_ID ? SYSTEM_TENANT_ID : '')
  await loadWhoami()
}

/**
 * Remounting the routed page on a tenant switch: every list it holds was
 * fetched for the previous tenant, and re-keying is cheaper to reason about
 * than a reload fan-out across pages.
 */
const viewKey = computed(() => `tenant:${tenantOverride.value}`)

function navigate(to: NavigateTarget) {
  void router
    .push({ name: to.name, params: to.params, query: to.query })
    .catch((error: unknown) => {
      if (!isSendplaneError(error)) console.error(error)
    })
}

function href(to: NavigateTarget): string {
  return router.resolve({ name: to.name, params: to.params, query: to.query }).href
}

// The system tenant cannot send, so the transactional send screen is not
// offered there (ADR-0017); the operator pages stay.
const navItems = computed(() =>
  navRoutes
    .filter((route) => !(systemView.value && route.name === 'messages.send'))
    .map((route) => ({ name: route.name, key: route.navKey! })),
)

const themeOptions: ThemeChoice[] = ['system', 'light', 'dark']
</script>

<template>
  <ApiKeyGate v-if="!apiKey" />

  <SendplaneProvider
    v-else
    :client="client"
    :navigate="navigate"
    :href="href"
    :locale="locale"
    :whoami="whoami"
    class="console"
  >
    <template #default="{ context }">
      <a class="console__skip" href="#main">Skip to content</a>

      <header class="console__bar">
        <strong class="console__brand">sendplane</strong>

        <nav class="console__nav" :aria-label="context.t('nav.campaigns')">
          <RouterLink
            v-for="item in navItems"
            :key="item.name"
            class="console__link"
            :to="{ name: item.name }"
          >
            {{ context.t(item.key) }}
          </RouterLink>
        </nav>

        <div class="console__tools">
          <label v-if="canSwitchTenant" class="console__tool">
            <span class="sp-visually-hidden">{{ context.t('tenant.switcher') }}</span>
            <select
              class="console__select"
              :value="tenantChoice"
              @change="switchTenant(($event.target as HTMLSelectElement).value)"
            >
              <option value="">
                {{ context.t('tenant.own', { tenant: ownTenantLabel }) }}
              </option>
              <option :value="SYSTEM_TENANT_ID">
                {{ context.t('tenant.system', { tenant: SYSTEM_TENANT_ID }) }}
              </option>
            </select>
          </label>

          <label class="console__tool">
            <span class="sp-visually-hidden">{{ context.t('common.locale') }}</span>
            <select
              class="console__select"
              :value="locale"
              @change="setLocale(($event.target as HTMLSelectElement).value)"
            >
              <option value="en">English</option>
              <option value="ko">한국어</option>
            </select>
          </label>

          <label class="console__tool">
            <span class="sp-visually-hidden">Theme</span>
            <select
              class="console__select"
              :value="theme"
              @change="setTheme(($event.target as HTMLSelectElement).value as ThemeChoice)"
            >
              <option v-for="option in themeOptions" :key="option" :value="option">
                {{ option }}
              </option>
            </select>
          </label>

          <button type="button" class="console__signout" @click="clearApiKey()">Sign out</button>
        </div>
      </header>

      <p v-if="systemView" class="console__banner" role="status">
        <strong>{{ context.t('tenant.systemBanner') }}</strong>
        <span>{{ context.t('tenant.systemBannerHint') }}</span>
      </p>

      <main id="main" class="console__main">
        <RouterView :key="viewKey" />
      </main>
    </template>
  </SendplaneProvider>
</template>

<style scoped>
.console {
  display: flex;
  flex-direction: column;
  min-height: 100vh;
}

.console__skip {
  position: absolute;
  top: -100px;
  left: var(--sp-space-2);
  z-index: 100;
  padding: var(--sp-space-2);
  background: var(--sp-surface);
  border: 1px solid var(--sp-border-strong);
  border-radius: var(--sp-radius-sm);
}

.console__skip:focus {
  top: var(--sp-space-2);
}

.console__bar {
  position: sticky;
  top: 0;
  z-index: 20;
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-3);
  align-items: center;
  padding: var(--sp-space-2) var(--sp-space-4);
  background: var(--sp-surface);
  border-bottom: 1px solid var(--sp-border);
}

.console__brand {
  font-size: var(--sp-font-size-lg);
  letter-spacing: -0.01em;
}

.console__nav {
  display: flex;
  flex: 1;
  flex-wrap: wrap;
  gap: var(--sp-space-1);
  min-width: 0;
  overflow-x: auto;
}

.console__link {
  padding: var(--sp-space-1) var(--sp-space-2);
  color: var(--sp-text-muted);
  font-size: var(--sp-font-size-sm);
  font-weight: 500;
  white-space: nowrap;
  text-decoration: none;
  border-radius: var(--sp-radius-sm);
}

.console__link:hover {
  color: var(--sp-text);
  background: var(--sp-surface-hover);
}

.console__link.router-link-active {
  color: var(--sp-accent);
  background: var(--sp-accent-soft);
}

.console__tools {
  display: flex;
  gap: var(--sp-space-2);
  align-items: center;
}

.console__select,
.console__signout {
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

.console__main {
  flex: 1;
}

.console__banner {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-space-2);
  align-items: baseline;
  margin: 0;
  padding: var(--sp-space-2) var(--sp-space-4);
  color: var(--sp-warn);
  font-size: var(--sp-font-size-sm);
  background: var(--sp-warn-soft);
  border-bottom: 1px solid var(--sp-border);
}
</style>
