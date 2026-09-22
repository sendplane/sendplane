import './theme.css'

import { defineAsyncComponent } from 'vue'

// Provider and context ------------------------------------------------------
export { default as SendplaneProvider } from './SendplaneProvider.vue'
export {
  createSendplaneContext,
  provideSendplane,
  SENDPLANE_KEY,
  useSendplane,
  useSendplaneOptional,
  type NavigateFn,
  type NavigateTarget,
  type ProvideSendplaneOptions,
  type SendplaneContext,
  type TranslateFn,
} from './context.js'

// Routing manifest ----------------------------------------------------------
export { navRoutes, resolvePath, routeByName, routes, type SendplaneRoute } from './routes.js'

// i18n ----------------------------------------------------------------------
export {
  bundledMessages,
  createMessages,
  mergeMessages,
  type LocaleMessages,
  type MessageSource,
  type MessageTree,
} from './i18n/index.js'

// Composables ---------------------------------------------------------------
export { useAsync, type UseAsyncOptions, type UseAsyncResult } from './composables/useAsync.js'
export {
  useCursorList,
  type CursorPage,
  type UseCursorList,
  type UseCursorListOptions,
} from './composables/useCursorList.js'
export {
  createConfirmApi,
  provideConfirmApi,
  useConfirm,
  type ConfirmApi,
  type ConfirmRequest,
} from './composables/useConfirm.js'
export { useApiToast } from './composables/useApiToast.js'
export {
  createToastApi,
  describeError,
  provideToastApi,
  useToast,
  type Toast,
  type ToastApi,
  type ToastKind,
} from './composables/useToast.js'

// Primitives ----------------------------------------------------------------
export { default as SpButton } from './components/SpButton.vue'
export { default as SpCard } from './components/SpCard.vue'
export { default as SpCheckbox } from './components/SpCheckbox.vue'
export { default as SpCodeEditor } from './components/SpCodeEditor.vue'
export { default as SpConfirmDialog } from './components/SpConfirmDialog.vue'
export { default as SpEmptyState } from './components/SpEmptyState.vue'
export { default as SpErrorNotice } from './components/SpErrorNotice.vue'
export { default as SpField } from './components/SpField.vue'
export { default as SpInput } from './components/SpInput.vue'
export { default as SpJsonView } from './components/SpJsonView.vue'
export { default as SpLink } from './components/SpLink.vue'
export { default as MailboxHealthBadge } from './components/MailboxHealthBadge.vue'
export { default as MailboxTestPanel } from './components/MailboxTestPanel.vue'
export { default as SpMultiFilter } from './components/SpMultiFilter.vue'
export { default as SpPageHeader } from './components/SpPageHeader.vue'
export { default as SpRecipientUpload } from './components/SpRecipientUpload.vue'
export { default as SpSelect, type SelectOption } from './components/SpSelect.vue'
export { default as SpSharedBadge } from './components/SpSharedBadge.vue'
export { default as SpStat } from './components/SpStat.vue'
export { default as SpStatusBadge } from './components/SpStatusBadge.vue'
export { default as SpTable, type TableColumn } from './components/SpTable.vue'
export { default as SpTabs, type TabItem } from './components/SpTabs.vue'
export { default as SpTextarea } from './components/SpTextarea.vue'
export { default as SpBlockEditorSlot } from './components/SpBlockEditorSlot.vue'
export { default as TenantVarsEditor } from './components/TenantVarsEditor.vue'

/**
 * The GrapesJS + grapesjs-mjml block editor (ADR-0009).
 *
 * Exported through `defineAsyncComponent` on purpose: a static re-export would
 * pull GrapesJS and the MJML compiler into whatever chunk imports this entry
 * point, which is every screen in the console. `SpBlockEditorSlot` is the seam
 * the template editor actually uses; this export is for a host that wants to
 * mount the editor on its own screen.
 */
export const MjmlBlockEditor = defineAsyncComponent(
  () => import('./components/MjmlBlockEditor.vue'),
)

// Pages ---------------------------------------------------------------------
export { default as CampaignListPage } from './pages/CampaignListPage.vue'
export { default as CampaignDetailPage } from './pages/CampaignDetailPage.vue'
export { default as DeliveryListPage } from './pages/DeliveryListPage.vue'
export { default as MessageSendPage } from './pages/MessageSendPage.vue'
export { default as DeliveryDetailPage } from './pages/DeliveryDetailPage.vue'
export { default as TemplateListPage } from './pages/TemplateListPage.vue'
export { default as TemplateEditorPage } from './pages/TemplateEditorPage.vue'
export { default as LayoutListPage } from './pages/LayoutListPage.vue'
export { default as LayoutEditorPage } from './pages/LayoutEditorPage.vue'
export { default as TransportListPage } from './pages/TransportListPage.vue'
export { default as TransportEditPage } from './pages/TransportEditPage.vue'
export { default as SenderListPage } from './pages/SenderListPage.vue'
export { default as SenderEditPage } from './pages/SenderEditPage.vue'
export { default as SendingDomainListPage } from './pages/SendingDomainListPage.vue'
export { default as ProbeMailboxListPage } from './pages/ProbeMailboxListPage.vue'
export { default as BounceMailboxListPage } from './pages/BounceMailboxListPage.vue'
export { default as SuppressionListPage } from './pages/SuppressionListPage.vue'
export { default as EventListPage } from './pages/EventListPage.vue'
export { default as SettingsPage } from './pages/SettingsPage.vue'

// Helpers -------------------------------------------------------------------
export {
  buildI18nMatrix,
  collectLocales,
  fallbackChain,
  type I18nMatrix,
  type I18nMatrixCell,
  type I18nMatrixRow,
} from './lib/i18n-matrix.js'
export {
  downloadText,
  formatBytes,
  formatDateTime,
  formatKeyValueLines,
  formatNumber,
  formatRate,
  formatRelative,
  parseKeyValueLines,
  safeJson,
  shortId,
  splitLines,
  tryParseJson,
} from './lib/format.js'
export { labelKeyFor, toneFor, type StatusKind, type Tone } from './lib/status.js'
export {
  extractTenantVarKeys,
  isFromDomainNotOwned,
  isPlatformReadOnly,
  isSenderUseDenied,
  isTenantVarsMissing,
  isTransportNotAssignable,
  looksTemplated,
  missingTenantVarKeys,
  renderTenantTemplate,
  type TenantTemplateRender,
} from './lib/platform.js'
export {
  BLOCK_EDITOR,
  BLOCK_EDITOR_VERSION,
  blockDefinitions,
  DEFAULT_MJML,
  extractI18nKeys,
  finishExport,
  i18nTag,
  isMjmlDocument,
  makeBlockProject,
  readBlockProject,
  replaceI18nKey,
  unescapeLiquid,
  type BlockDefinition,
  type BlockEditorProject,
  type BlockEditorValue,
} from './lib/mjml-blocks.js'
