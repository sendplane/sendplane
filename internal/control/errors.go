package control

import (
	"errors"
	"fmt"

	"github.com/sendplane/sendplane/store"
)

// The errors the synchronous campaign helpers return. The HTTP layer maps
// ErrInvalidTransition to 409 and the precondition errors to 422.
var (
	// ErrInvalidTransition is returned when the campaign state machine of
	// docs/architecture.md 4.1 / 7.1 does not allow the requested move.
	ErrInvalidTransition = errors.New("control: invalid campaign transition")
	// ErrNoRecipients is returned by StartCampaign when nothing was ingested.
	ErrNoRecipients = errors.New("control: campaign has no recipients")
	// ErrNoSender is returned by StartCampaign when the campaign names no
	// sender, or one that does not exist.
	ErrNoSender = errors.New("control: campaign sender is missing")
	// ErrNoVersion is returned by StartCampaign when the campaign was never
	// bound to a published MessageVersion.
	ErrNoVersion = errors.New("control: campaign has no message version")
)

// invalidTransition builds the ErrInvalidTransition message for one campaign.
func invalidTransition(c *store.Campaign, action string) error {
	return fmt.Errorf("%w: cannot %s campaign %s in status %s",
		ErrInvalidTransition, action, c.ID, c.Status)
}
