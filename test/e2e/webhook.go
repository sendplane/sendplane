package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// webhookSecret must match SENDPLANE_WEBHOOK_SECRET in
// test/e2e/docker-compose.yml. It is a fixed public string on purpose: the
// stack is thrown away after every run.
//
//nolint:gosec // G101: a fixed, public, test-only secret; the stack is torn down after every run.
const webhookSecret = "sendplane-e2e-webhook-secret"

// receivedEvent is one host.Event as cmd/sendplane's webhookSink serializes
// it, plus whether its batch's HMAC verified.
type receivedEvent struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Payload  json.RawMessage `json:"payload"`
	Signed   bool            `json:"-"`
	Received time.Time       `json:"-"`
}

// eventSink is the tiny HTTP receiver of scenario 7. It runs in the harness
// process on the host; the control container reaches it through
// host.docker.internal (docker-compose.yml `extra_hosts`), which is why
// events.webhook.allow_private_networks has to be on in config.yaml.
type eventSink struct {
	srv    *http.Server
	addr   string
	secret []byte

	mu       sync.Mutex
	events   []receivedEvent
	batches  int
	unsigned int
}

func newEventSink(listen string) (*eventSink, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("event sink cannot listen on %s: %w", listen, err)
	}
	s := &eventSink{addr: ln.Addr().String(), secret: []byte(webhookSecret)}
	mux := http.NewServeMux()
	mux.HandleFunc("/hooks", s.handle)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Println("event sink stopped:", err)
		}
	}()
	return s, nil
}

func (s *eventSink) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	signed := hmac.Equal([]byte(want), []byte(r.Header.Get("X-Sendplane-Signature")))

	var batch []receivedEvent
	if err := json.Unmarshal(body, &batch); err != nil {
		// Answering 200 anyway would hide a payload change behind an empty
		// event list; 400 makes the dispatcher retry and eventually dead-letter
		// it, which scenario 7 also asserts on.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.batches++
	if !signed {
		s.unsigned++
	}
	now := time.Now()
	for _, e := range batch {
		e.Signed, e.Received = signed, now
		s.events = append(s.events, e)
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// url is what the control container must be configured with. The harness only
// reports it; the value itself comes from the compose file, because the
// container cannot learn it at runtime.
func (s *eventSink) url() string { return "http://host.docker.internal:" + portOf(s.addr) + "/hooks" }

func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return port
}

// byType returns every event of a type received so far.
func (s *eventSink) byType(typ string) []receivedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []receivedEvent
	for _, e := range s.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (s *eventSink) counts() (events, batches, unsigned int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events), s.batches, s.unsigned
}

// waitFor blocks until at least one event of typ has arrived.
func (s *eventSink) waitFor(ctx context.Context, typ string, timeout time.Duration) ([]receivedEvent, error) {
	deadline := time.Now().Add(timeout)
	for {
		if evs := s.byType(typ); len(evs) > 0 {
			return evs, nil
		}
		if time.Now().After(deadline) {
			s.mu.Lock()
			seen := map[string]int{}
			for _, e := range s.events {
				seen[e.Type]++
			}
			s.mu.Unlock()
			return nil, fmt.Errorf("no %s event after %s (received: %v)", typ, timeout, seen)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *eventSink) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
}
