import { routes as manifest } from '@sendplane/ui'
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'

/**
 * The console owns the router; `@sendplane/ui` only publishes the manifest
 * (ADR-0010). Every route's path parameters are declared as the page's props,
 * so `props: true` is all the wiring that is needed.
 */
const records: RouteRecordRaw[] = manifest.map((route) => ({
  name: route.name,
  path: route.path,
  component: route.page,
  props: true,
}))

records.push(
  { path: '/', redirect: { name: 'campaigns' } },
  // A path redirect rather than a named one: redirecting by name would carry
  // the catch-all's `pathMatch` param into a route that does not declare it,
  // which vue-router warns about on every unknown URL.
  { path: '/:pathMatch(.*)*', redirect: '/campaigns' },
)

export const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: records,
  scrollBehavior: () => ({ top: 0 }),
})
