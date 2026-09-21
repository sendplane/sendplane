// Command e2e drives the end-to-end test of docs/architecture.md 15 against
// the stack in test/e2e/docker-compose.yml (postgres, control, two senders,
// bounce, chaos-smtp, GreenMail):
//
//	docker build -f deploy/dev/Dockerfile -t sendplane:dev .
//	docker compose -f test/e2e/docker-compose.yml up -d
//	go run ./test/e2e
//
// Eight scenarios run in order, each printing PASS, FAIL or KNOWN-FAIL with
// its own timing. A scenario collects every failed assertion instead of
// stopping at the first one, because a whole compose stack is expensive enough
// that finding one difference per run would waste it.
//
// KNOWN-FAIL marks an assertion that is correct and that the product currently
// does not satisfy. It is reported loudly and listed in test/e2e/README.md
// with the file:line of the cause, but it does not fail the run — the
// assertion stays in so that fixing the bug turns the line green by itself.
// Pass --strict to make known failures fatal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sendplane/sendplane/internal/chaossmtp"
)

type options struct {
	api            string
	chaosStats     string
	greenmailAPI   string
	greenmailSMTP  string
	webhookListen  string
	trackingDomain string

	recipients  int
	chunk       int
	seed        uint64
	tempFail    float64
	permFail    float64
	drop        float64
	maxAttempts int

	budget       time.Duration
	poll         time.Duration
	probeTimeout time.Duration
	mailTimeout  time.Duration

	killSender   bool
	killAt       float64
	killDowntime time.Duration
	composeFile  string
	composeCmd   string
	dockerCmd    string

	only   string
	strict bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("e2e", flag.ContinueOnError)
	var o options

	fs.StringVar(&o.api, "api", "http://127.0.0.1:18180", "base URL of the control API")
	fs.StringVar(&o.chaosStats, "chaos-stats", "http://127.0.0.1:19190", "base URL of the chaos-smtp stats listener")
	fs.StringVar(&o.greenmailAPI, "greenmail-api", "http://127.0.0.1:18581", "base URL of the GreenMail REST API")
	fs.StringVar(&o.greenmailSMTP, "greenmail-smtp", "127.0.0.1:13325", "GreenMail SMTP address the harness injects bounces through")
	fs.StringVar(&o.webhookListen, "webhook-listen", "0.0.0.0:18585",
		"address the harness's event receiver binds; must match SENDPLANE_WEBHOOK_URL in the compose file")
	fs.StringVar(&o.trackingDomain, "tracking-domain", "t.e2e.test", "tenant tracking domain")

	fs.IntVar(&o.recipients, "recipients", 10_000, "recipients in the bulk campaign of scenario 2")
	fs.IntVar(&o.chunk, "chunk", 5_000, "recipients per NDJSON chunk")
	fs.Uint64Var(&o.seed, "seed", 42, "chaos-smtp seed; must match the compose file")
	fs.Float64Var(&o.tempFail, "tempfail", 0.05, "chaos-smtp tempfail rate; must match the compose file")
	fs.Float64Var(&o.permFail, "permfail", 0.01, "chaos-smtp permfail rate; must match the compose file")
	fs.Float64Var(&o.drop, "drop", 0.005, "chaos-smtp drop rate; must match the compose file")
	fs.IntVar(&o.maxAttempts, "max-attempts", 4, "tenant retry policy max_attempts")

	fs.DurationVar(&o.budget, "budget", 10*time.Minute, "the whole run must finish inside this")
	fs.DurationVar(&o.poll, "poll", 2*time.Second, "campaign polling interval")
	fs.DurationVar(&o.probeTimeout, "probe-timeout", 3*time.Minute, "how long scenario 6 waits for a probe verdict")
	fs.DurationVar(&o.mailTimeout, "mail-timeout", 90*time.Second, "how long to wait for a message to reach GreenMail")

	fs.BoolVar(&o.killSender, "kill-sender", false, "SIGKILL one sender replica during the bulk campaign and restart it")
	fs.Float64Var(&o.killAt, "kill-at", 0.10, "progress fraction at which the sender is killed")
	fs.DurationVar(&o.killDowntime, "kill-downtime", 10*time.Second, "how long the killed replica stays down")
	fs.StringVar(&o.composeFile, "compose-file", "test/e2e/docker-compose.yml", "compose file used for the kill/restart step")
	fs.StringVar(&o.composeCmd, "compose-cmd", "docker compose",
		"how Compose is invoked; use \"docker-compose\" where only the standalone binary is installed")
	fs.StringVar(&o.dockerCmd, "docker-cmd", "docker", "docker CLI used for the SIGKILL")

	fs.StringVar(&o.only, "only", "", "comma-separated scenario numbers to run (bootstrap always runs)")
	fs.BoolVar(&o.strict, "strict", false, "treat KNOWN-FAIL assertions as failures")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if o.recipients < 1 || o.chunk < 1 {
		return options{}, errors.New("--recipients and --chunk must be positive")
	}
	if o.chunk > o.recipients {
		o.chunk = o.recipients
	}
	return o, nil
}

func run(args []string) error {
	opt, err := parseFlags(args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The whole run, not one request, is bounded: a stuck campaign has to fail
	// the job rather than hang until the CI timeout.
	ctx, cancel := context.WithTimeout(ctx, opt.budget)
	defer cancel()

	sink, err := newEventSink(opt.webhookListen)
	if err != nil {
		return err
	}
	defer sink.close()

	r := &runner{
		opt:     opt,
		api:     newAPIClient(opt.api, 5*time.Minute),
		gm:      newGreenmail(opt.greenmailAPI, opt.greenmailSMTP),
		hc:      &http.Client{Timeout: 30 * time.Second},
		sink:    sink,
		started: time.Now(),
	}
	r.logf("event receiver listening on %s (control must be pointed at %s)", sink.addr, sink.url())

	runErr := r.execute(ctx)
	r.printSummary()

	if runErr != nil {
		return runErr
	}
	if n := r.hardFailures(); n > 0 {
		return fmt.Errorf("%d scenario(s) failed", n)
	}
	return nil
}

// --- runner -------------------------------------------------------------

type result struct {
	name    string
	took    time.Duration
	fails   []string
	known   []string
	skipped bool
	aborted error
}

func (s result) verdict() string {
	switch {
	case s.skipped:
		return "SKIP"
	case s.aborted != nil || len(s.fails) > 0:
		return "FAIL"
	case len(s.known) > 0:
		return "KNOWN-FAIL"
	default:
		return "PASS"
	}
}

type runner struct {
	opt     options
	api     *apiClient
	gm      *greenmail
	hc      *http.Client
	sink    *eventSink
	started time.Time

	results []result
	cur     *result

	// state shared between scenarios, filled by the bootstrap
	trackingKeyKID    string
	trackingKeySecret []byte
	transportChaosID  string
	transportMailID   string
	senderChaosID     string
	senderMailID      string
	domainMailID      string
	templateID        string
	versionID         string
	links             []string

	bulkCampaignID  string
	trackCampaignID string
	trackDeliveries []delivery
	probeRunID      string
	probeMailboxID  string
	probeCollected  bool
	exp             expectation
}

func (r *runner) elapsed() time.Duration { return time.Since(r.started).Round(time.Millisecond) }

func (r *runner) logf(format string, a ...any) {
	fmt.Printf("[%8s] %s\n", r.elapsed(), fmt.Sprintf(format, a...))
}

// fail records an assertion failure without stopping the scenario.
func (r *runner) fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	r.cur.fails = append(r.cur.fails, msg)
	r.logf("  ASSERT FAILED: %s", msg)
}

// knownFail records an assertion that is right and that the product currently
// does not satisfy. bug is the entry in test/e2e/README.md's known-issue table.
//
// Nothing calls it right now - the four entries that table used to have are
// all fixed - and it stays anyway, because it is the mechanism the README
// documents and --strict switches: the next assertion that is right before
// the product is marks itself with this instead of being deleted or softened.
//
//nolint:unused // the KNOWN-FAIL mechanism of the README, kept for the next one.
func (r *runner) knownFail(bug, format string, a ...any) {
	msg := fmt.Sprintf("[%s] %s", bug, fmt.Sprintf(format, a...))
	if r.opt.strict {
		r.cur.fails = append(r.cur.fails, msg)
		r.logf("  ASSERT FAILED: %s", msg)
		return
	}
	r.cur.known = append(r.cur.known, msg)
	r.logf("  KNOWN-FAIL: %s", msg)
}

func (r *runner) scenario(ctx context.Context, name string, fn func(context.Context) error) {
	if !r.selected(name) {
		r.results = append(r.results, result{name: name, skipped: true})
		r.logf("== %s: skipped by --only", name)
		return
	}
	r.logf("== %s", name)
	res := result{name: name}
	r.cur = &res
	start := time.Now()
	if err := fn(ctx); err != nil {
		res.aborted = err
		r.logf("  ABORTED: %v", err)
	}
	res.took = time.Since(start).Round(time.Millisecond)
	r.results = append(r.results, res)
	r.logf("== %s %s in %s", name, res.verdict(), res.took)
	r.cur = nil
}

// selected implements --only. The scenario names start with their number, so
// "--only=4,5" matches "4. tracking" and "5. bounce"; the bootstrap always
// runs because everything else depends on what it creates.
func (r *runner) selected(name string) bool {
	if r.opt.only == "" || strings.HasPrefix(name, "1.") {
		return true
	}
	num, _, _ := strings.Cut(name, ".")
	for _, want := range strings.Split(r.opt.only, ",") {
		if strings.TrimSpace(want) == num {
			return true
		}
	}
	return false
}

func (r *runner) hardFailures() int {
	n := 0
	for _, s := range r.results {
		if s.aborted != nil || len(s.fails) > 0 {
			n++
		}
	}
	return n
}

func (r *runner) printSummary() {
	fmt.Println()
	fmt.Println("=== sendplane e2e summary =========================================")
	for _, s := range r.results {
		fmt.Printf("%-10s %-46s %8s\n", s.verdict(), s.name, s.took)
		if s.aborted != nil {
			fmt.Printf("           aborted: %v\n", s.aborted)
		}
		for _, f := range s.fails {
			fmt.Printf("           FAIL: %s\n", f)
		}
		for _, k := range s.known {
			fmt.Printf("           KNOWN-FAIL: %s\n", k)
		}
	}
	events, batches, unsigned := r.sink.counts()
	fmt.Printf("\nwebhook: %d event(s) in %d batch(es), %d with a bad signature\n", events, batches, unsigned)
	fmt.Printf("total: %s\n", r.elapsed())
	fmt.Println("===================================================================")
}

func (r *runner) rates() chaossmtp.Rates {
	return chaossmtp.Rates{
		TempFailRate: r.opt.tempFail,
		PermFailRate: r.opt.permFail,
		DropRate:     r.opt.drop,
	}
}

func (r *runner) execute(ctx context.Context) error {
	r.logf("e2e: %d bulk recipients, budget %s, kill-sender=%v", r.opt.recipients, r.opt.budget, r.opt.killSender)

	r.scenario(ctx, "1. bootstrap", r.scenarioBootstrap)
	if len(r.results[0].fails) > 0 || r.results[0].aborted != nil {
		return errors.New("bootstrap failed; the remaining scenarios cannot run")
	}
	r.scenario(ctx, "3. transactional + idempotency", r.scenarioTransactional)
	r.scenario(ctx, "4. tracking and unsubscribe", r.scenarioTracking)
	r.scenario(ctx, "5. bounce, complaint, forged DSN", r.scenarioBounce)
	// Scenario 8 (lease recovery) rides along inside the bulk campaign.
	r.scenario(ctx, "2. bulk campaign", r.scenarioBulkCampaign)
	r.scenario(ctx, "6. loopback probe", r.scenarioProbe)
	r.scenario(ctx, "7. events", r.scenarioEvents)
	return nil
}

// --- compose helpers (scenario 8) ---------------------------------------

// killAndRestartSender SIGKILLs one sender replica and brings it back, so the
// run covers the lease recovery path of architecture 15.1. The dead replica's
// rows stay leased until sender.lease_for expires and the control lease reaper
// releases them. Same approach as test/load/main.go.
func (r *runner) killAndRestartSender(ctx context.Context) error {
	ids, err := r.composeOutput(ctx, "ps", "-q", "sender")
	if err != nil {
		return err
	}
	lines := strings.Fields(ids)
	if len(lines) == 0 {
		return errors.New("no running sender container found")
	}
	victim := lines[len(lines)-1]

	r.logf("  killing sender container %s with SIGKILL", victim[:min(12, len(victim))])
	//nolint:gosec // G204: the docker command and container name are this
	// harness's own flags and its own compose project, not request input.
	if out, err := exec.CommandContext(ctx, r.opt.dockerCmd, "kill", "--signal=KILL", victim).CombinedOutput(); err != nil {
		return fmt.Errorf("docker kill: %w: %s", err, strings.TrimSpace(string(out)))
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.opt.killDowntime):
	}

	r.logf("  restarting the killed sender replica")
	if _, err := r.composeOutput(ctx, "up", "-d", "--no-recreate", "sender"); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}
	return nil
}

// composeOutput shells out to Compose. The command is a flag because the
// plugin form ("docker compose") and the standalone binary ("docker-compose")
// are both in the wild and the harness has no way to guess which is installed.
func (r *runner) composeOutput(ctx context.Context, args ...string) (string, error) {
	argv := strings.Fields(r.opt.composeCmd)
	if len(argv) == 0 {
		return "", errors.New("--compose-cmd is empty")
	}
	full := make([]string, 0, len(argv)+2+len(args))
	full = append(full, argv[1:]...)
	full = append(full, "-f", r.opt.composeFile)
	full = append(full, args...)
	//nolint:gosec // G204: argv and the compose file come from this harness's own flags.
	out, err := exec.CommandContext(ctx, argv[0], full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", argv[0], strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
