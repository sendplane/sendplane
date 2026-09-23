package host

import (
	"context"
	"errors"
	"testing"
)

func TestDefaultTemplatePolicy(t *testing.T) {
	ctx := context.Background()
	shared := TemplateUse{TemplateKey: "welcome", Shared: true, Uses: []UseKind{UseTransactional}}

	shared.Kind = UseTransactional
	if err := DefaultTemplatePolicy(ctx, shared); err != nil {
		t.Fatalf("a listed use was denied: %v", err)
	}
	shared.Kind = UseCampaign
	if err := DefaultTemplatePolicy(ctx, shared); !errors.Is(err, ErrTemplateUseDenied) {
		t.Fatalf("an unlisted use of a shared template: got %v, want ErrTemplateUseDenied", err)
	}
	// A tenant's own template (an override included) is not restricted by
	// its copied `uses`, and an empty list restricts nothing.
	own := shared
	own.Shared = false
	if err := DefaultTemplatePolicy(ctx, own); err != nil {
		t.Fatalf("a tenant's own template was restricted: %v", err)
	}
	open := TemplateUse{Shared: true, Kind: UseCampaign}
	if err := DefaultTemplatePolicy(ctx, open); err != nil {
		t.Fatalf("an unrestricted shared template was denied: %v", err)
	}
}
