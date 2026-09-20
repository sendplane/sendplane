package api

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/internal/chaossmtp"
	"github.com/sendplane/sendplane/internal/sender"
	"github.com/sendplane/sendplane/store"
)

// A campaign's deliveries are ingested as pending and nothing ever promotes
// them in bulk: they become claimable when the campaign joins the sender's
// running set (store.DeliveryRepo.Claim, ADR-0002). That is a contract shared
// between the HTTP layer, the control plane and the sender, so it is only
// really tested end to end: create, ingest, start, and watch a real sender
// loop carry the mail to a real SMTP server.
//
// Without it, a regression in any one of the three looks like "the campaign is
// running" and sends nothing at all.
func TestCampaignSendsThroughARealSender(t *testing.T) {
	smtpd, err := chaossmtp.Start(chaossmtp.Options{})
	if err != nil {
		t.Fatalf("chaossmtp: %v", err)
	}
	defer func() { _ = smtpd.Close() }()

	e := newEnv(t)
	snd := e.seedSenderFor(smtpd.Addr())
	tpl := e.seedTemplate()
	e.do(http.MethodPost, "/api/v1/templates/"+tpl.Id.String()+"/publish", nil)

	camp := decodeInto[Campaign](t, e.do(http.MethodPost, "/api/v1/campaigns", CampaignInput{
		Name: "spring", SenderId: *snd.Id, TemplateId: tpl.Id, DefaultLocale: ptr("en"),
	}), http.StatusCreated)

	const recipients = 5
	var lines strings.Builder
	for i := range recipients {
		lines.WriteString(`{"email":"r` + strconv.Itoa(i) + `@example.com","name":"R"}` + "\n")
	}
	ingested := decodeInto[RecipientIngestResult](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/recipients", strings.NewReader(lines.String()),
		withHeader("Content-Type", "application/x-ndjson")), http.StatusOK)
	if ingested.Accepted != recipients {
		t.Fatalf("accepted %d, want %d", ingested.Accepted, recipients)
	}

	// Ingested rows are pending, and a pending row is not claimable until its
	// campaign is running: that is the whole point of the CampaignIDs rule.
	ctx := t.Context()
	counts, err := e.st.Deliveries().CountByStatus(ctx, camp.Id.String())
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if counts[store.DeliveryPending] != recipients {
		t.Fatalf("after ingest: %v, want %d pending", counts, recipients)
	}

	started := decodeInto[Campaign](t, e.do(http.MethodPost,
		"/api/v1/campaigns/"+camp.Id.String()+"/start", nil), http.StatusOK)
	if started.Status != CampaignStatusRunning {
		t.Fatalf("status after start = %q, want running", started.Status)
	}

	// A real sender loop, claiming from the same store the HTTP layer wrote to.
	snd2, err := sender.New(e.provider, sender.Config{
		WorkerID: "w1",
		Lanes:    map[store.Lane]int{store.LaneBulk: 2},
		// Short intervals so the test does not sit through the production
		// three-second tenant scan.
		PollInterval:    20 * time.Millisecond,
		CampaignRefresh: 20 * time.Millisecond,
		LeaseFor:        time.Minute,
	})
	if err != nil {
		t.Fatalf("sender.New: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- snd2.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("sender.Run: %v", err)
		}
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		counts, err = e.st.Deliveries().CountByStatus(ctx, camp.Id.String())
		if err != nil {
			t.Fatalf("CountByStatus: %v", err)
		}
		if counts[store.DeliverySent] == recipients {
			break
		}
		if time.Now().After(deadline) {
			list, _ := e.st.Deliveries().ListByCampaign(ctx, camp.Id.String(), store.DeliveryFilter{}, store.Page{Limit: 5})
			reason := ""
			if len(list.Items) > 0 {
				reason = list.Items[0].LastError
			}
			t.Fatalf("deliveries never reached sent: %v (chaossmtp accepted %d, last error %q)",
				counts, smtpd.Stats().Accepted, reason)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := smtpd.Stats().Accepted; got != recipients {
		t.Errorf("chaossmtp accepted %d messages, want %d", got, recipients)
	}
	// The mail really went through the render path, not a stub.
	msgs := smtpd.Messages()
	if len(msgs) == 0 || !strings.Contains(msgs[0].Subject, "Hello") {
		t.Errorf("subject = %q, want the rendered greeting", msgs[0].Subject)
	}
}

// seedSenderFor is seedSender pointed at a live SMTP server: plain TCP, no
// auth, which is what chaossmtp offers by default.
func (e *env) seedSenderFor(addr string) Sender {
	e.t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		e.t.Fatalf("split %q: %v", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		e.t.Fatalf("port %q: %v", port, err)
	}
	tr := decodeInto[Transport](e.t, e.do(http.MethodPost, "/api/v1/transports", TransportInput{
		Name: "chaos", Host: host, Port: int32(p), Tls: ptr(TLSMode(store.TLSNone)),
	}), http.StatusCreated)
	return decodeInto[Sender](e.t, e.do(http.MethodPost, "/api/v1/senders", SenderInput{
		Name: "marketing", FromEmail: "news@example.com", FromName: ptr("Example"),
		TransportId: *tr.Id,
	}), http.StatusCreated)
}
