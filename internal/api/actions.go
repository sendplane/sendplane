package api

import "github.com/sendplane/sendplane/host"

// Code in this file is the compile-time table architecture 16 asks for: every
// operation of api/openapi.yaml names the Action its caller must hold, and a
// route that is missing from the table is refused rather than served without
// an authorization check.
//
// The keys are the operation IDs the generated strict server hands to a
// StrictMiddlewareFunc, which are the spec's operationId values with an
// upper-case first letter. TestEveryOperationHasAnAction re-reads the spec and
// fails if the two ever drift apart.

// publicOps are the operations whose spec entry clears `security` and declares
// `x-sendplane-action: none`: the tracking routes a mail client hits and the
// liveness probe. They skip Authenticate, the tenant resolver and Authorize.
// ProbeInbound is the one entry the generated strict server never asks about:
// api/oapi-codegen.yaml excludes the operation and internal/api/probeinbound.go
// mounts it by hand. It is listed anyway so that the table still describes
// every operation of the spec, which is what TestEveryOperationHasAnAction
// checks.
var publicOps = map[string]bool{
	"GetServiceHealth":      true,
	"OneClickUnsubscribe":   true,
	"ProbeInbound":          true,
	"TrackClick":            true,
	"TrackOpen":             true,
	"TrackUnsubscribeClick": true,
}

// authOnlyOps are the operations that declare `x-sendplane-action: none`
// while keeping `security`: they authenticate the caller, resolve its tenant
// and then call no Authorizer at all.
//
// There is exactly one, and it is deliberately hard to add to. A console has
// to know who it is talking to and which tenant it is looking at before it can
// render anything, and gating that on a permission turns a missing role into a
// blank page (GET /api/v1/whoami). Anything that returns tenant *data* belongs
// in opActions instead.
var authOnlyOps = map[string]bool{
	"GetWhoami": true,
}

// opActions maps every authenticated operation to the Action a principal needs
// for it. resourceKindFor names the object the Action is checked against.
var opActions = map[string]host.Action{
	"CancelCampaign":               host.ActionCampaignSend,
	"CreateBounceMailbox":          host.ActionSenderWrite,
	"CreateCampaign":               host.ActionCampaignWrite,
	"CreateLayout":                 host.ActionTemplateWrite,
	"CreateProbeMailbox":           host.ActionSenderWrite,
	"CreateSender":                 host.ActionSenderWrite,
	"CreateSendingDomain":          host.ActionSenderWrite,
	"CreateTemplate":               host.ActionTemplateWrite,
	"CreateTransport":              host.ActionSenderWrite,
	"DeleteBounceMailbox":          host.ActionSenderWrite,
	"DeleteCampaign":               host.ActionCampaignWrite,
	"DeleteLayout":                 host.ActionTemplateWrite,
	"DeleteProbeMailbox":           host.ActionSenderWrite,
	"DeleteSender":                 host.ActionSenderWrite,
	"DeleteSendingDomain":          host.ActionSenderWrite,
	"DeleteSuppression":            host.ActionSuppressionWrite,
	"DeleteTemplate":               host.ActionTemplateWrite,
	"DeleteTransport":              host.ActionSenderWrite,
	"GetBounce":                    host.ActionDeliveryRead,
	"GetBounceMailbox":             host.ActionSenderRead,
	"GetCampaign":                  host.ActionCampaignRead,
	"GetDelivery":                  host.ActionDeliveryRead,
	"GetEvent":                     host.ActionEventRead,
	"GetLayout":                    host.ActionTemplateRead,
	"GetMessageVersion":            host.ActionTemplateRead,
	"GetProbeMailbox":              host.ActionSenderRead,
	"GetProbeRun":                  host.ActionSenderRead,
	"GetSender":                    host.ActionSenderRead,
	"GetSenderHealth":              host.ActionSenderRead,
	"GetSendingDomain":             host.ActionSenderRead,
	"GetSuppression":               host.ActionSuppressionRead,
	"GetTemplate":                  host.ActionTemplateRead,
	"GetTemplateI18n":              host.ActionTemplateRead,
	"GetTenantSettings":            host.ActionSettingsRead,
	"GetTransport":                 host.ActionSenderRead,
	"GetTransportHealth":           host.ActionSenderRead,
	"IngestCampaignRecipients":     host.ActionCampaignWrite,
	"ListBounces":                  host.ActionDeliveryRead,
	"ListBounceMailboxes":          host.ActionSenderRead,
	"ListCampaignDeliveries":       host.ActionDeliveryRead,
	"ListCampaignLinks":            host.ActionCampaignRead,
	"ListCampaigns":                host.ActionCampaignRead,
	"ListDeadLetterEvents":         host.ActionEventRead,
	"ListDeliveries":               host.ActionDeliveryRead,
	"ListDeliveryAttempts":         host.ActionDeliveryRead,
	"ListDeliveryBounces":          host.ActionDeliveryRead,
	"ListEvents":                   host.ActionEventRead,
	"ListLayouts":                  host.ActionTemplateRead,
	"ListMessageVersions":          host.ActionTemplateRead,
	"ListProbeMailboxes":           host.ActionSenderRead,
	"ListProbeRuns":                host.ActionSenderRead,
	"ListSenders":                  host.ActionSenderRead,
	"ListSendingDomains":           host.ActionSenderRead,
	"ListSuppressions":             host.ActionSuppressionRead,
	"ListTemplateI18nKeys":         host.ActionTemplateRead,
	"ListTemplates":                host.ActionTemplateRead,
	"ListTransports":               host.ActionSenderRead,
	"NotifyCampaignUnsubscribe":    host.ActionCampaignWrite,
	"NotifyDeliveryUnsubscribe":    host.ActionDeliveryWrite,
	"PauseCampaign":                host.ActionCampaignSend,
	"PreviewTemplate":              host.ActionTemplateRead,
	"PublishTemplate":              host.ActionTemplateWrite,
	"ReplaceTemplateI18n":          host.ActionTemplateWrite,
	"ReplayEvent":                  host.ActionEventWrite,
	"ResumeCampaign":               host.ActionCampaignSend,
	"RetryCampaignDeliveries":      host.ActionCampaignSend,
	"RetryDelivery":                host.ActionDeliveryWrite,
	"SendMessage":                  host.ActionMessageSend,
	"StartCampaign":                host.ActionCampaignSend,
	"TestBounceMailbox":            host.ActionSenderWrite,
	"TestBounceMailboxCredentials": host.ActionSenderWrite,
	"TestProbeMailbox":             host.ActionSenderWrite,
	"TestProbeMailboxCredentials":  host.ActionSenderWrite,
	"TriggerProbeRun":              host.ActionSenderWrite,
	"UpdateBounceMailbox":          host.ActionSenderWrite,
	"UpdateCampaign":               host.ActionCampaignWrite,
	"UpdateLayout":                 host.ActionTemplateWrite,
	"UpdateProbeMailbox":           host.ActionSenderWrite,
	"UpdateSender":                 host.ActionSenderWrite,
	"UpdateSendingDomain":          host.ActionSenderWrite,
	"UpdateTemplate":               host.ActionTemplateWrite,
	"UpdateTenantSettings":         host.ActionSettingsWrite,
	"UpdateTransport":              host.ActionSenderWrite,
	"UpsertSuppression":            host.ActionSuppressionWrite,
}

// resourceKindFor is the Resource.Kind an Authorizer sees, derived from the
// Action so that a host can write "may this role touch senders" without
// knowing 79 operation names. The ID is filled in by the middleware from the
// request's path parameter when there is one.
func resourceKindFor(a host.Action) string {
	switch a {
	case host.ActionSettingsRead, host.ActionSettingsWrite:
		return "settings"
	case host.ActionSenderRead, host.ActionSenderWrite:
		return "sender"
	case host.ActionTemplateRead, host.ActionTemplateWrite:
		return "template"
	case host.ActionCampaignRead, host.ActionCampaignWrite, host.ActionCampaignSend:
		return "campaign"
	case host.ActionMessageSend:
		return "message"
	case host.ActionDeliveryRead, host.ActionDeliveryWrite:
		return "delivery"
	case host.ActionSuppressionRead, host.ActionSuppressionWrite:
		return "suppression"
	case host.ActionEventRead, host.ActionEventWrite:
		return "event"
	}
	return ""
}
