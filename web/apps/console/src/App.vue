<script setup lang="ts">
import { createClient, isSendplaneError } from '@sendplane/api'
import { navRoutes, SendplaneProvider, type NavigateTarget } from '@sendplane/ui'
import { computed } from 'vue'
import { useRouter } from 'vue-router'

import ApiKeyGate from './ApiKeyGate.vue'
import {
  apiKey,
  authHeaders,
  baseUrl,
  clearApiKey,
  locale,
  setLocale,
  setTheme,
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

const navItems = computed(() =>
  navRoutes.map((route) => ({ name: route.name, key: route.navKey! })),
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

      <main id="main" class="console__main">
        <RouterView />
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
</style>
