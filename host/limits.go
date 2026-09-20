package host

// Limits bounds request sizes. A zero field takes the DefaultLimits value.
type Limits struct {
	MaxRecipientsPerCampaign int   // recipients accepted for one campaign
	MaxVarsBytes             int   // JSON size of one recipient's vars
	MaxBodyBytes             int64 // template/layout body size
	MaxRecipientLineBytes    int   // one NDJSON ingest line
}

// DefaultLimits are applied to any zero field of Limits.
var DefaultLimits = Limits{
	MaxRecipientsPerCampaign: 10_000_000,
	MaxVarsBytes:             8 << 10,
	MaxBodyBytes:             2 << 20,
	MaxRecipientLineBytes:    64 << 10,
}

// WithDefaults returns l with every zero field replaced by the DefaultLimits
// value. It is exported because the packages that consume Limits
// (internal/ingest, internal/api) are handed the host's struct directly and
// must apply the same defaults the root package applies.
func (l Limits) WithDefaults() Limits {
	d := DefaultLimits
	if l.MaxRecipientsPerCampaign == 0 {
		l.MaxRecipientsPerCampaign = d.MaxRecipientsPerCampaign
	}
	if l.MaxVarsBytes == 0 {
		l.MaxVarsBytes = d.MaxVarsBytes
	}
	if l.MaxBodyBytes == 0 {
		l.MaxBodyBytes = d.MaxBodyBytes
	}
	if l.MaxRecipientLineBytes == 0 {
		l.MaxRecipientLineBytes = d.MaxRecipientLineBytes
	}
	return l
}
