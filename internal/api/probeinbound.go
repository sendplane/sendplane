package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sendplane/sendplane/internal/mbhealth"
	"github.com/sendplane/sendplane/internal/probe"
	"github.com/sendplane/sendplane/internal/probe/inbound"
	"github.com/sendplane/sendplane/store"
)

// POST {path} — the inbound webhook of ADR-0016, one route per configured
// provider. It is the second public surface after the tracking routes, and it
// is built the same way: no Authenticate, no tenant resolver, a per-IP token
// bucket, a hard body cap, and a tenant that comes out of a signed token on
// the payload rather than out of the request.
//
// It is mounted by hand rather than through the generated strict server, which
// `api/oapi-codegen.yaml` excludes it from. Two reasons, both structural: the
// route is mounted once per *configured* provider at a path the host may
// override, and the handler needs the request itself — the exact bytes the
// signature covers, plus the header that carries it — which a decoded request
// object cannot give it.

// ProbeInboundRoute is one configured inbound webhook, already resolved
// against internal/probe/inbound's registry by whoever built the Deps.
type ProbeInboundRoute struct {
	// Path is the route to mount, e.g. "/probe/inbound/sendplane".
	Path string
	// Provider parses and authenticates the request.
	Provider inbound.Provider
	// Secrets are what the provider's signature is checked against; any one
	// matching is enough, so that a secret can be rotated without a window
	// in which deliveries are refused.
	Secrets []string
}

// ProbeCompleter is the half of internal/probe the inbound webhook needs. Like
// ProbeTrigger it is an interface, so a handler test needs no probe runner and
// no DNS resolver; unlike ProbeTrigger it names probe.Evidence, because the
// whole point of the type is that both inbound channels hand the verdict the
// same thing.
type ProbeCompleter interface {
	// VerifyToken reports whether an X-Sendplane-Probe value belongs to this
	// tenant's run.
	VerifyToken(tenantID, runID, token string) bool
	// CompleteRun writes the verdict for a run whose probe mail arrived.
	CompleteRun(ctx context.Context, st store.Store, run *store.ProbeRun, ev probe.Evidence, now time.Time) error
	// RefreshSenderHealth recomputes the sender summary and emits
	// sender.health_changed when it moved.
	RefreshSenderHealth(ctx context.Context, st store.Store, senderID string, now time.Time) error
}

// seenProviderIDs is how many delivery IDs are remembered for idempotency.
// A redelivery follows a lost response within seconds to minutes, and a
// deployment that probes six-hourly sees a handful of messages an hour, so a
// few hundred entries cover it many times over. The map is bounded rather than
// expiring: it is a best-effort shortcut, and the real idempotency is
// ProbeRun.Pending, which is in the store.
const seenProviderIDs = 512

// deliveryMemo remembers the provider delivery IDs already handled, so a
// webhook redelivered after the run completed does not even read the store.
type deliveryMemo struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
}

func newDeliveryMemo() *deliveryMemo {
	return &deliveryMemo{seen: make(map[string]struct{}, seenProviderIDs)}
}

// has reports whether an ID has already been handled.
func (m *deliveryMemo) has(id string) bool {
	if id == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, dup := m.seen[id]
	return dup
}

// add records an ID. It is called only once the delivery has actually been
// dealt with — never before — because a delivery that ended in a deferral is
// one the provider will send again, and remembering it would turn that retry
// into a no-op and lose the probe.
func (m *deliveryMemo) add(id string) {
	if id == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.seen[id]; dup {
		return
	}
	m.seen[id] = struct{}{}
	m.order = append(m.order, id)
	if len(m.order) > seenProviderIDs {
		delete(m.seen, m.order[0])
		m.order = m.order[1:]
	}
}

// probeInboundHandler serves one configured route.
func (s *server) probeInboundHandler(route ProbeInboundRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !s.limit.allow(ip) {
			w.Header().Set("Retry-After", "1")
			writeError(w, newErr(http.StatusTooManyRequests, ErrorCodeRateLimited, "too many requests"))
			return
		}

		// The body is read before anything else and capped independently of
		// Limits.MaxBodyBytes: the signature covers the raw bytes, so they
		// have to exist in full before they can be trusted at all, and an
		// unauthenticated caller must not be able to make that allocation
		// large.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, inbound.MaxBodyBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, newErr(http.StatusRequestEntityTooLarge, ErrorCodeLimitExceeded,
					"inbound webhook body is over %d bytes", inbound.MaxBodyBytes))
				return
			}
			// The connection went away mid-body. There is nothing to verify
			// and nothing to answer with that the sender will read.
			s.deps.Logger.Warn("sendplane: inbound probe webhook body could not be read",
				"provider", route.Provider.Name(), "remote_ip", ip, "err", err)
			writeError(w, newErr(http.StatusBadRequest, ErrorCodeInvalidRequest,
				"could not read the request body"))
			return
		}

		if err := route.Provider.Verify(r, body, route.Secrets); err != nil {
			// Logged with the address, because this is the one thing on this
			// route that an operator has to be able to see: either the secret
			// was rotated on one side only, or somebody is guessing.
			s.deps.Logger.Warn("sendplane: inbound probe webhook signature rejected",
				"provider", route.Provider.Name(), "path", route.Path, "remote_ip", ip, "err", err)
			writeError(w, newErr(http.StatusUnauthorized, ErrorCodeUnauthenticated,
				"the inbound webhook signature did not verify"))
			return
		}

		msgs, err := route.Provider.Parse(body)
		if err != nil {
			// The signature verified, so this really is our sender; the
			// payload is simply not the format. Retrying the same bytes
			// cannot fix that, so it is a 400 and not a deferral.
			s.deps.Logger.Warn("sendplane: inbound probe webhook payload rejected",
				"provider", route.Provider.Name(), "err", err)
			writeError(w, newErr(http.StatusBadRequest, ErrorCodeInvalidRequest,
				"the inbound webhook payload is not the %s format: %s",
				route.Provider.Name(), err))
			return
		}

		// A request carries one message for the format sendplane ships and
		// may carry several for a batching one, so the outcomes are folded:
		// one message that could not be recorded makes the whole request a
		// deferral, because that is the only lever there is — a sender
		// retries requests, not messages. Otherwise any message that
		// completed a run makes it an acceptance, and a request where nothing
		// was ours is ignored.
		outcome := inbound.OutcomeIgnored
		for i := range msgs {
			switch s.handleInboundMessage(r.Context(), route, msgs[i]) {
			case inbound.OutcomeDeferred:
				replyInbound(w, inbound.OutcomeDeferred)
				return
			case inbound.OutcomeAccepted:
				outcome = inbound.OutcomeAccepted
			case inbound.OutcomeIgnored:
			}
		}
		replyInbound(w, outcome)
	}
}

// replyInbound writes the outcome. The mapping is here and not in a provider
// so that every format answers the same way, and so that no format can invent
// a code that makes a sender drop probe mail:
//
//	accepted -> 200   recorded, or already recorded
//	ignored  -> 200   not a sendplane probe; a 4xx would make a sender retry
//	                  forever a message sendplane will never want
//	deferred -> 503   could not record it; hold the mail and retry
//
// A sender that turns a 5xx into an SMTP deferral at its own MX is the reason
// "the store is down" must not be a 200: the alternative is a bounce for a
// probe that was never judged.
func replyInbound(w http.ResponseWriter, outcome inbound.Outcome) {
	status := http.StatusOK
	if outcome == inbound.OutcomeDeferred {
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "60")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(outcome.String() + "\n"))
}

// handleInboundMessage completes at most one probe run.
func (s *server) handleInboundMessage(
	ctx context.Context, route ProbeInboundRoute, msg inbound.Message,
) inbound.Outcome {
	log := s.deps.Logger.With("provider", route.Provider.Name(),
		"delivery", msg.ProviderID, "from", msg.From)

	token := strings.TrimSpace(msg.Header(probe.HeaderProbe))
	if token == "" {
		// Not a sendplane probe — a webhook pointed at a shared address will
		// see plenty of those. The subject carries the run ID too, but not the
		// tenant, and this endpoint has nothing else to resolve a tenant from,
		// so a probe mail that lost its header cannot be attributed here. Say
		// so rather than sweep every tenant looking for the run.
		if runID, ok := probeRunIDFromSubject(msg.Subject()); ok {
			log.Warn("sendplane: a probe mail arrived with no "+probe.HeaderProbe+
				" header; the inbound webhook cannot resolve its tenant", "run", runID)
		}
		return inbound.OutcomeIgnored
	}
	if s.probeSeen.has(msg.ProviderID) {
		return inbound.OutcomeAccepted
	}

	tenantID, runID, ok := probe.ParseToken(token)
	if !ok {
		log.Warn("sendplane: inbound probe token is malformed")
		return inbound.OutcomeIgnored
	}
	st, err := s.deps.Provider.ForTenant(ctx, tenantID)
	if err != nil {
		// An unknown tenant is a forged or stale token, not an outage; a store
		// that is down is an outage. Telling them apart matters, because one
		// must make the provider retry and the other must not.
		if errors.Is(err, store.ErrNotFound) {
			log.Warn("sendplane: inbound probe names an unknown tenant", "tenant", tenantID)
			return inbound.OutcomeIgnored
		}
		log.Error("sendplane: inbound probe could not open the tenant store",
			"tenant", tenantID, "err", err)
		return inbound.OutcomeDeferred
	}

	run, err := st.ProbeRuns().Get(ctx, runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Retention removed it, or the token was made up. Either way there
			// is nothing to complete and nothing to retry.
			return inbound.OutcomeIgnored
		}
		log.Error("sendplane: inbound probe could not read its run", "run", runID, "err", err)
		return inbound.OutcomeDeferred
	}
	if !s.deps.ProbeCompleter.VerifyToken(tenantID, runID, token) {
		log.Warn("sendplane: inbound probe token did not verify", "tenant", tenantID, "run", runID)
		return inbound.OutcomeIgnored
	}
	if !run.Pending {
		// Already finished: a redelivery, or the IMAP collector got there
		// first. Accepting is what stops the provider retrying.
		s.probeSeen.add(msg.ProviderID)
		return inbound.OutcomeAccepted
	}

	box, err := st.ProbeMailboxes().Get(ctx, run.MailboxID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Error("sendplane: inbound probe could not read its mailbox",
			"mailbox", run.MailboxID, "err", err)
		return inbound.OutcomeDeferred
	}

	headers := probe.HeadersFromMap(msg.Headers)
	now := s.now()
	ev := probe.Evidence{
		Mailbox: box,
		Headers: headers,
		// No folder: the provider reports a delivery, not where the message
		// was later filed. verdict.go treats that as neutral rather than as
		// "not the inbox" (ADR-0016).
		Folder: "",
		// ReceivedAt is left zero: no inbound format reports an arrival time
		// of its own, so internal/probe falls back to the newest Received
		// header, which is the receiving MTA's own clock and the closest
		// thing a forwarded message has to one.
		RawHeaders: headers.String(),
	}
	if err := s.deps.ProbeCompleter.CompleteRun(ctx, st, run, ev, now); err != nil {
		log.Error("sendplane: inbound probe could not complete its run", "run", runID, "err", err)
		return inbound.OutcomeDeferred
	}

	// From here on the verdict is saved. Nothing below may turn the answer
	// into a deferral: the provider would redeliver and the work would be
	// redone for no gain.
	if err := s.deps.ProbeCompleter.RefreshSenderHealth(ctx, st, run.SenderID, now); err != nil {
		log.Error("sendplane: inbound probe could not refresh sender health",
			"sender", run.SenderID, "err", err)
	}
	if box != nil {
		// A webhook mailbox has no login, so an arriving probe is the only
		// evidence there is that the forward still works (architecture 11.5).
		if _, err := mbhealth.Record(ctx, st, mbhealth.KindProbe,
			mbhealth.Mailbox{ID: box.ID, Name: box.Name, Health: box.Health},
			mbhealth.OK(), now); err != nil {
			log.Warn("sendplane: recording probe mailbox health failed",
				"mailbox", box.ID, "err", err)
		}
	}
	s.probeSeen.add(msg.ProviderID)
	log.Info("sendplane: inbound probe completed a run",
		"tenant", tenantID, "run", runID, "status", run.Status.String())
	return inbound.OutcomeAccepted
}

// probeRunIDFromSubject reads the run ID back out of "[sendplane probe <id>]".
func probeRunIDFromSubject(subject string) (string, bool) {
	const prefix = "[sendplane probe "
	i := strings.Index(subject, prefix)
	if i < 0 {
		return "", false
	}
	rest := subject[i+len(prefix):]
	id, _, ok := strings.Cut(rest, "]")
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
