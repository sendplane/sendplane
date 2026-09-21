// Command chaos-smtp runs internal/chaossmtp as a standalone process: an
// ESMTP server that fails deterministically, used by load and e2e tests as a
// stand-in for a hostile relay (docs/architecture.md 15). It is a thin flag
// wrapper; all behaviour lives in internal/chaossmtp.
//
// A GET /stats, GET /messages and POST /reset endpoint run on a second
// listener so a test driver can read counters, inspect what the relay actually
// received, and reset state between runs without restarting the process (the
// server has no in-package Reset, so /reset works by stopping the running
// chaossmtp.Server and starting a fresh one with the same options: same
// behaviour, empty state).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sendplane/sendplane/internal/chaossmtp"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "chaos-smtp:", err)
		os.Exit(1)
	}
}

// keepMessagesOption maps the --keep-messages flag onto
// chaossmtp.Options.KeepMessages, whose zero value means "keep everything".
func keepMessagesOption(n int) int {
	if n == 0 {
		return chaossmtp.KeepNone
	}
	if n < 0 {
		return 0 // the library's "keep every message"
	}
	return n
}

func run(args []string) error {
	fs := flag.NewFlagSet("chaos-smtp", flag.ContinueOnError)

	listen := fs.String("listen", "127.0.0.1:2525", "SMTP listen address")
	statsListen := fs.String("stats-listen", ":9090", "address for GET /stats, GET /messages and POST /reset")
	tempFail := fs.Float64("tempfail", 0, "probability [0,1) of a 451 temporary failure")
	permFail := fs.Float64("permfail", 0, "probability [0,1) of a 550 permanent failure")
	drop := fs.Float64("drop", 0, "probability [0,1) of dropping the connection mid-DATA")
	latency := fs.Duration("latency", 0, "latency added before every end-of-DATA reply")
	seed := fs.Uint64("seed", 0, "seed selecting the deterministic failure sequence")
	rateLimitAfter := fs.Int("ratelimit-after", 0, "hang up with 421 after this many accepted messages on one connection (0 disables)")
	keepMessages := fs.Int("keep-messages", 0,
		"how many accepted messages to remember for GET /messages: 0 keeps none (counters only), n>0 keeps the most recent n, -1 keeps every message")
	keepBodies := fs.Bool("keep-bodies", false,
		"also keep each remembered message's body, so GET /messages?body=1 can return it (costs the size of every message kept)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := chaossmtp.Options{
		Addr: *listen,
		Rates: chaossmtp.Rates{
			TempFailRate: *tempFail,
			PermFailRate: *permFail,
			DropRate:     *drop,
		},
		RateLimitAfter: *rateLimitAfter,
		Latency:        *latency,
		Seed:           *seed,
		// The library keeps every message, which is right for a test that
		// asserts on them and wrong for a process that runs for an hour
		// against a million-recipient campaign: nothing reads Messages()
		// here, so the default is to keep none.
		KeepMessages: keepMessagesOption(*keepMessages),
		KeepBodies:   *keepBodies,
	}

	host, err := newChaosHost(opts)
	if err != nil {
		return err
	}
	defer host.close()

	log.Printf("chaos-smtp: listening on %s (tempfail=%.4f permfail=%.4f drop=%.4f seed=%d)",
		host.addr(), *tempFail, *permFail, *drop, *seed)

	statsServer := &http.Server{
		Addr:    *statsListen,
		Handler: host.statsMux(),
		// A stats endpoint has no reason to let a client hold a connection
		// open sending headers one byte at a time (gosec G112).
		ReadHeaderTimeout: 10 * time.Second,
	}
	statsErrCh := make(chan error, 1)
	go func() {
		log.Printf("chaos-smtp: stats listening on %s", *statsListen)
		if err := statsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			statsErrCh <- err
			return
		}
		statsErrCh <- nil
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	<-ctx.Done()

	log.Print("chaos-smtp: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = statsServer.Shutdown(shutdownCtx)
	return <-statsErrCh
}

// messageList is the GET /messages body: what the relay received, newest
// last, which is the order chaossmtp.Server.Messages returns them in.
type messageList struct {
	Total int             `json:"total"`
	Items []recordedEntry `json:"items"`
}

// recordedEntry is one chaossmtp.Message on the wire. The headers are a plain
// map of the first value per name, because a JSON client asking "what subject
// did this recipient get" does not want textproto's slices; Body is only
// present when the caller asked for it and the server was started with
// --keep-bodies.
type recordedEntry struct {
	From       string            `json:"from"`
	Rcpts      []string          `json:"rcpts"`
	MessageID  string            `json:"message_id,omitempty"`
	Subject    string            `json:"subject,omitempty"`
	Attempt    int               `json:"attempt,omitempty"`
	Size       int               `json:"size"`
	ReceivedAt time.Time         `json:"received_at"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body,omitempty"`
}

// selectMessages applies the rcpt filter and the limit, newest last. An empty
// rcpt matches everything; a limit of zero means "no limit", and a positive
// one keeps the most recent matches, which is what a test that only wants to
// look at a sample asks for.
func selectMessages(msgs []chaossmtp.Message, rcpt string, limit int, withBody bool) []recordedEntry {
	rcpt = strings.ToLower(strings.TrimSpace(rcpt))
	out := make([]recordedEntry, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		if rcpt != "" && !hasRcpt(m.Rcpts, rcpt) {
			continue
		}
		e := recordedEntry{
			From: m.From, Rcpts: m.Rcpts, MessageID: m.MessageID,
			Subject: m.Subject, Attempt: m.Attempt, Size: m.Size,
			ReceivedAt: m.ReceivedAt, Headers: map[string]string{},
		}
		for name := range m.Headers {
			e.Headers[name] = m.Headers.Get(name)
		}
		if withBody {
			e.Body = string(m.Body)
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func hasRcpt(rcpts []string, want string) bool {
	for _, r := range rcpts {
		if strings.EqualFold(strings.TrimSpace(r), want) {
			return true
		}
	}
	return false
}

// intParam parses an optional non-negative integer query parameter.
func intParam(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return n, nil
}

// chaosHost owns the running chaossmtp.Server and lets /reset replace it with
// a fresh instance bound to the same address, since chaossmtp.Server itself
// has no Reset method (internal/chaossmtp is out of scope for this binary).
type chaosHost struct {
	opts chaossmtp.Options

	mu  sync.RWMutex
	srv *chaossmtp.Server
}

func newChaosHost(opts chaossmtp.Options) (*chaosHost, error) {
	srv, err := chaossmtp.Start(opts)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	// Pin the resolved address (useful when --listen uses :0) so /reset
	// rebinds to the same port instead of a new random one.
	opts.Addr = srv.Addr()
	return &chaosHost{opts: opts, srv: srv}, nil
}

func (h *chaosHost) addr() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.srv.Addr()
}

func (h *chaosHost) stats() chaossmtp.Stats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.srv.Stats()
}

func (h *chaosHost) messages() []chaossmtp.Message {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.srv.Messages()
}

// reset stops the current server and starts a new one with the same options,
// so accumulated Stats/Messages are cleared between test runs.
func (h *chaosHost) reset() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.srv.Close(); err != nil {
		return err
	}
	srv, err := chaossmtp.Start(h.opts)
	if err != nil {
		return err
	}
	h.srv = srv
	return nil
}

func (h *chaosHost) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = h.srv.Close()
}

func (h *chaosHost) statsMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.stats())
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		limit, err := intParam(q.Get("limit"))
		if err != nil {
			http.Error(w, "limit: "+err.Error(), http.StatusBadRequest)
			return
		}
		out := selectMessages(h.messages(), q.Get("rcpt"), limit, q.Get("body") == "1")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(messageList{Total: len(out), Items: out})
	})
	mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := h.reset(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
