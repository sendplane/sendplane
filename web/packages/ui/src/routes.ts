import type { Component } from 'vue'

/**
 * One screen of the console.
 *
 * This package owns the *names*; the host owns the router. A host turns the
 * manifest into vue-router routes (or its own framework's equivalent) and the
 * pages navigate by name through `navigate({ name, params })`, so nothing here
 * ever imports vue-router (ADR-0010).
 */
export interface SendplaneRoute {
  /** Stable identifier used by `navigate` and `href`. */
  name: string
  /** Suggested path, with `:param` placeholders matching the page's props. */
  path: string
  /** Lazy loader, so a host that mounts one page does not pull in the rest. */
  page: () => Promise<{ default: Component }>
  /** Path parameters the page takes as props, in the order they appear. */
  params: readonly string[]
  /** Message key for a nav label, when the route belongs in the sidebar. */
  navKey?: string
}

export const routes: readonly SendplaneRoute[] = [
  {
    name: 'campaigns',
    path: '/campaigns',
    page: () => import('./pages/CampaignListPage.vue'),
    params: [],
    navKey: 'nav.campaigns',
  },
  {
    name: 'campaign',
    path: '/campaigns/:campaignId',
    page: () => import('./pages/CampaignDetailPage.vue'),
    params: ['campaignId'],
  },
  {
    name: 'campaign.deliveries',
    path: '/campaigns/:campaignId/deliveries',
    page: () => import('./pages/DeliveryListPage.vue'),
    params: ['campaignId'],
  },
  {
    name: 'deliveries',
    path: '/deliveries',
    page: () => import('./pages/DeliveryListPage.vue'),
    params: [],
    navKey: 'nav.deliveries',
  },
  {
    name: 'delivery',
    path: '/deliveries/:deliveryId',
    page: () => import('./pages/DeliveryDetailPage.vue'),
    params: ['deliveryId'],
  },
  {
    name: 'templates',
    path: '/templates',
    page: () => import('./pages/TemplateListPage.vue'),
    params: [],
    navKey: 'nav.templates',
  },
  {
    name: 'template.new',
    path: '/templates/new',
    page: () => import('./pages/TemplateEditorPage.vue'),
    params: [],
  },
  {
    name: 'template',
    path: '/templates/:templateId',
    page: () => import('./pages/TemplateEditorPage.vue'),
    params: ['templateId'],
  },
  {
    name: 'layouts',
    path: '/layouts',
    page: () => import('./pages/LayoutListPage.vue'),
    params: [],
    navKey: 'nav.layouts',
  },
  {
    name: 'layout.new',
    path: '/layouts/new',
    page: () => import('./pages/LayoutEditorPage.vue'),
    params: [],
  },
  {
    name: 'layout',
    path: '/layouts/:layoutId',
    page: () => import('./pages/LayoutEditorPage.vue'),
    params: ['layoutId'],
  },
  {
    name: 'transports',
    path: '/transports',
    page: () => import('./pages/TransportListPage.vue'),
    params: [],
    navKey: 'nav.transports',
  },
  {
    name: 'transport.new',
    path: '/transports/new',
    page: () => import('./pages/TransportEditPage.vue'),
    params: [],
  },
  {
    name: 'transport',
    path: '/transports/:transportId',
    page: () => import('./pages/TransportEditPage.vue'),
    params: ['transportId'],
  },
  {
    name: 'senders',
    path: '/senders',
    page: () => import('./pages/SenderListPage.vue'),
    params: [],
    navKey: 'nav.senders',
  },
  {
    name: 'sender.new',
    path: '/senders/new',
    page: () => import('./pages/SenderEditPage.vue'),
    params: [],
  },
  {
    name: 'sender',
    path: '/senders/:senderId',
    page: () => import('./pages/SenderEditPage.vue'),
    params: ['senderId'],
  },
  {
    name: 'sending-domains',
    path: '/sending-domains',
    page: () => import('./pages/SendingDomainListPage.vue'),
    params: [],
    navKey: 'nav.sendingDomains',
  },
  {
    name: 'probe-mailboxes',
    path: '/probe-mailboxes',
    page: () => import('./pages/ProbeMailboxListPage.vue'),
    params: [],
    navKey: 'nav.probeMailboxes',
  },
  {
    name: 'suppressions',
    path: '/suppressions',
    page: () => import('./pages/SuppressionListPage.vue'),
    params: [],
    navKey: 'nav.suppressions',
  },
  {
    name: 'events',
    path: '/events',
    page: () => import('./pages/EventListPage.vue'),
    params: [],
    navKey: 'nav.events',
  },
  {
    name: 'settings',
    path: '/settings',
    page: () => import('./pages/SettingsPage.vue'),
    params: [],
    navKey: 'nav.settings',
  },
]

const byName = new Map(routes.map((route) => [route.name, route]))

export function routeByName(name: string): SendplaneRoute | undefined {
  return byName.get(name)
}

/** Routes meant for a sidebar, in the order §13 recommends working through. */
export const navRoutes: readonly SendplaneRoute[] = routes.filter((route) => route.navKey)

/**
 * Fills a route's path placeholders. Used by the default `href` so that links
 * are anchors even when the host has no router; a host with a router should
 * pass its own resolver instead.
 */
export function resolvePath(name: string, params?: Record<string, string | number>): string {
  const route = byName.get(name)
  if (!route) throw new Error(`[@sendplane/ui] unknown route name: ${name}`)
  return route.path.replace(/:([A-Za-z0-9_]+)/g, (_match, key: string) => {
    const value = params?.[key]
    if (value === undefined) {
      throw new Error(`[@sendplane/ui] route ${name} needs the "${key}" parameter`)
    }
    return encodeURIComponent(String(value))
  })
}
