package host_test

import (
	"testing"

	"github.com/sendplane/sendplane/host"
)

func TestWithDefaultsFillsEveryZeroField(t *testing.T) {
	if got := (host.Limits{}).WithDefaults(); got != host.DefaultLimits {
		t.Fatalf("zero Limits.WithDefaults() = %+v, want %+v", got, host.DefaultLimits)
	}
}

func TestWithDefaultsKeepsSetFields(t *testing.T) {
	l := host.Limits{MaxVarsBytes: 1024}.WithDefaults()
	if l.MaxVarsBytes != 1024 {
		t.Fatalf("MaxVarsBytes = %d, want the value that was set (1024)", l.MaxVarsBytes)
	}
	if l.MaxRecipientsPerCampaign != host.DefaultLimits.MaxRecipientsPerCampaign {
		t.Fatalf("MaxRecipientsPerCampaign = %d, want the default %d",
			l.MaxRecipientsPerCampaign, host.DefaultLimits.MaxRecipientsPerCampaign)
	}
	if l.MaxBodyBytes != host.DefaultLimits.MaxBodyBytes {
		t.Fatalf("MaxBodyBytes = %d, want the default %d", l.MaxBodyBytes, host.DefaultLimits.MaxBodyBytes)
	}
	if l.MaxRecipientLineBytes != host.DefaultLimits.MaxRecipientLineBytes {
		t.Fatalf("MaxRecipientLineBytes = %d, want the default %d",
			l.MaxRecipientLineBytes, host.DefaultLimits.MaxRecipientLineBytes)
	}
}

// The Action constants are the closed set api/openapi.yaml's x-sendplane-action
// draws from. A renamed constant is a breaking change for a host's RBAC
// mapping, so the string values are pinned here.
func TestActionStrings(t *testing.T) {
	for want, got := range map[string]host.Action{
		"settings.read":         host.ActionSettingsRead,
		"settings.write":        host.ActionSettingsWrite,
		"sender.read":           host.ActionSenderRead,
		"sender.write":          host.ActionSenderWrite,
		"template.read":         host.ActionTemplateRead,
		"template.write":        host.ActionTemplateWrite,
		"campaign.read":         host.ActionCampaignRead,
		"campaign.write":        host.ActionCampaignWrite,
		"campaign.send":         host.ActionCampaignSend,
		"message.send":          host.ActionMessageSend,
		"delivery.read":         host.ActionDeliveryRead,
		"delivery.write":        host.ActionDeliveryWrite,
		"suppression.read":      host.ActionSuppressionRead,
		"suppression.write":     host.ActionSuppressionWrite,
		"event.read":            host.ActionEventRead,
		"event.write":           host.ActionEventWrite,
		"probe.run":             host.ActionProbeRun,
		"tracking.config.write": host.ActionTrackingConfigWrite,
	} {
		if string(got) != want {
			t.Errorf("Action = %q, want %q", got, want)
		}
	}
}
