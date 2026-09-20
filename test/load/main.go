// Command loadgen drives the 1M-recipient load test of
// docs/architecture.md 15.1 against the stack in test/load/docker-compose.yml:
// it configures a tenant, streams N recipients in NDJSON chunks, starts the
// campaign, polls until it completes, SIGKILLs a sender replica on the way to
// exercise lease recovery, and then asserts the result against counts derived
// from chaossmtp's decision function rather than from a recorded snapshot.
//
//	docker build -f deploy/dev/Dockerfile -t sendplane:dev .
//	docker compose -f test/load/docker-compose.yml up -d
//	go run ./test/load --recipients=100000 --kill-sender
//
// Every failed assertion is collected and reported together, and the process
// exits non-zero if there is at least one: a load run is expensive enough that
// stopping at the first difference would waste it.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sendplane/sendplane/internal/chaossmtp"
)

type options struct {
	api        string
	chaosStats string
	dsn        string

	recipients  int
	chunk       int
	replayChunk int
	domain      string

	ingestBudget time.Duration
	budget       time.Duration
	poll         time.Duration

	killSender   bool
	killAt       float64
	killDowntime time.Duration
	composeFile  string
	composeCmd   string
	dockerCmd    string

	seed        uint64
	tempFail    float64
	permFail    float64
	drop        float64
	maxAttempts int

	trackingDomain string
	sample         int
	reportPath     string
	summaryPath    string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opt, err := parseFlags(args)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The whole run, not just one request, is bounded by the budget: a stuck
	// campaign has to fail the job rather than hang until the CI timeout.
	ctx, cancel := context.WithTimeout(ctx, opt.budget)
	defer cancel()

	r := &runner{
		opt:     opt,
		api:     newAPIClient(opt.api, 10*time.Minute),
		hc:      &http.Client{Timeout: 30 * time.Second},
		started: time.Now(),
		rep:     newReport(opt),
	}
	runErr := r.execute(ctx)
	if runErr != nil {
		// A run that aborted is a failed run, and the summary has to say so
		// rather than reporting PASS on the assertions that did get to run.
		r.rep.Failures = append(r.rep.Failures, "run aborted: "+runErr.Error())
	}

	// The report and the summary are written even when the run failed: the
	// counts are what explains the failure.
	if err := r.rep.write(opt.reportPath, opt.summaryPath); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen: writing the report failed:", err)
	}
	fmt.Print(r.rep.markdown())

	if runErr != nil {
		return runErr
	}
	if n := len(r.rep.Failures); n > 0 {
		return fmt.Errorf("%d assertion(s) failed", n)
	}
	return nil
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	var o options

	fs.StringVar(&o.api, "api", "http://127.0.0.1:18080", "base URL of the control API")
	fs.StringVar(&o.chaosStats, "chaos-stats", "http://127.0.0.1:19090", "base URL of the chaos-smtp stats listener")
	fs.StringVar(&o.dsn, "dsn", "postgres://sendplane:sendplane@127.0.0.1:15432/sendplane?sslmode=disable",
		"postgres DSN used for the database-size measurement; empty skips it")

	fs.IntVar(&o.recipients, "recipients", 1_000_000, "number of recipients to ingest and send to")
	fs.IntVar(&o.chunk, "chunk", 50_000, "recipients per NDJSON chunk")
	fs.IntVar(&o.replayChunk, "replay-chunk", 7, "index of the chunk re-sent to check idempotency (clamped to the last chunk)")
	fs.StringVar(&o.domain, "email-domain", "load.test", "recipient domain")

	fs.DurationVar(&o.ingestBudget, "ingest-budget", 3*time.Minute, "ingest must finish inside this")
	fs.DurationVar(&o.budget, "budget", 40*time.Minute, "the whole run must finish inside this")
	fs.DurationVar(&o.poll, "poll", 5*time.Second, "campaign polling interval")

	fs.BoolVar(&o.killSender, "kill-sender", false, "SIGKILL one sender replica mid-run and restart it")
	fs.Float64Var(&o.killAt, "kill-at", 0.10, "progress fraction at which the sender is killed")
	fs.DurationVar(&o.killDowntime, "kill-downtime", 10*time.Second, "how long the killed replica stays down")
	fs.StringVar(&o.composeFile, "compose-file", "test/load/docker-compose.yml", "compose file used for the kill/restart step")
	fs.StringVar(&o.composeCmd, "compose-cmd", "docker compose",
		"how Compose is invoked; use \"docker-compose\" where only the standalone binary is installed")
	fs.StringVar(&o.dockerCmd, "docker-cmd", "docker", "docker CLI used for the SIGKILL")

	fs.Uint64Var(&o.seed, "seed", 42, "chaos-smtp seed; must match the compose file")
	fs.Float64Var(&o.tempFail, "tempfail", 0.05, "chaos-smtp tempfail rate; must match the compose file")
	fs.Float64Var(&o.permFail, "permfail", 0.01, "chaos-smtp permfail rate; must match the compose file")
	fs.Float64Var(&o.drop, "drop", 0.005, "chaos-smtp drop rate; must match the compose file")
	fs.IntVar(&o.maxAttempts, "max-attempts", 6, "tenant retry policy max_attempts")

	fs.StringVar(&o.trackingDomain, "tracking-domain", "t.load.test", "tenant tracking domain")
	fs.IntVar(&o.sample, "sample", 200, "sample size for the failed-delivery and latency checks")
	fs.StringVar(&o.reportPath, "report", "report.json", "where the JSON report is written")
	fs.StringVar(&o.summaryPath, "summary", "", "file the Markdown summary is appended to (e.g. $GITHUB_STEP_SUMMARY)")

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

// runner holds the state one run accumulates.
type runner struct {
	opt     options
	api     *apiClient
	hc      *http.Client
	started time.Time
	rep     *report

	campaignID string
	versionID  string
	links      []string
	exp        expectation
}

func (r *runner) elapsed() time.Duration { return time.Since(r.started).Round(time.Millisecond) }

func (r *runner) logf(format string, a ...any) {
	fmt.Printf("[%8s] %s\n", r.elapsed(), fmt.Sprintf(format, a...))
}

// step runs one named phase and records how long it took.
func (r *runner) step(name string, f func() error) error {
	r.logf("=> %s", name)
	start := time.Now()
	err := f()
	d := time.Since(start)
	r.rep.Steps = append(r.rep.Steps, stepTiming{Name: name, Seconds: d.Seconds()})
	if err != nil {
		r.logf("<= %s failed after %s: %v", name, d.Round(time.Millisecond), err)
		return err
	}
	r.logf("<= %s in %s", name, d.Round(time.Millisecond))
	return nil
}

// fail records an assertion failure without stopping the run.
func (r *runner) fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	r.rep.Failures = append(r.rep.Failures, msg)
	r.logf("ASSERT FAILED: %s", msg)
}

func (r *runner) email(i int) string {
	return fmt.Sprintf("u%08d@%s", i, r.opt.domain)
}

func (r *runner) execute(ctx context.Context) error {
	r.logf("loadgen: %d recipients, chunk %d, budget %s, kill-sender=%v",
		r.opt.recipients, r.opt.chunk, r.opt.budget, r.opt.killSender)

	if err := r.step("wait for /healthz", func() error { return r.waitHealthy(ctx) }); err != nil {
		return err
	}
	if err := r.step("compute expected counts", func() error {
		r.exp = computeExpectation(r.opt.seed, r.rates(), r.email, r.opt.recipients, r.opt.maxAttempts)
		r.rep.Expected = r.exp
		r.logf("expected: sent=%d failed=%d (permanent=%d exhausted=%d) attempts=%d",
			r.exp.Sent, r.exp.Failed, r.exp.FailedPermanent, r.exp.FailedExhausted, r.exp.Attempts)
		return nil
	}); err != nil {
		return err
	}
	if err := r.step("configure tenant, transport, sender, template, campaign", func() error {
		return r.setup(ctx)
	}); err != nil {
		return err
	}
	if err := r.step("ingest recipients", func() error { return r.ingest(ctx) }); err != nil {
		return err
	}
	if err := r.step("start campaign", func() error {
		var c campaign
		return r.api.postJSON(ctx, "/api/v1/campaigns/"+r.campaignID+"/start", map[string]any{}, &c)
	}); err != nil {
		return err
	}
	if err := r.step("run campaign", func() error { return r.pollUntilDone(ctx) }); err != nil {
		return err
	}
	return r.step("assert", func() error { return r.assert(ctx) })
}

func (r *runner) rates() chaossmtp.Rates {
	return chaossmtp.Rates{
		TempFailRate: r.opt.tempFail,
		PermFailRate: r.opt.permFail,
		DropRate:     r.opt.drop,
	}
}

func (r *runner) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		// Any 200 counts. cmd/sendplane registers its own plain-text
		// /healthz on the root mux, which shadows the ServiceHealth JSON
		// that api/openapi.yaml documents, so the body is not parseable.
		err := r.api.getJSON(ctx, "/healthz", nil)
		if err == nil {
			r.logf("control is up")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("control never became healthy: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// setup creates everything the campaign needs. Tracking, the sendplane
// unsubscribe mode and suppression are all switched on so the 1M run exercises
// per-recipient link rewriting, pixel insertion and the suppression lookup
// (architecture 9.2) rather than a stripped-down send path.
func (r *runner) setup(ctx context.Context) error {
	var cur tenantSettings
	if err := r.api.getJSON(ctx, "/api/v1/settings", &cur); err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	yes := true
	// A short backoff: the default production schedule would park the ~5.5% of
	// deliveries that tempfail or drop for minutes and blow the budget without
	// testing anything the short one does not.
	update := map[string]any{
		"version": cur.Version,
		"retry": retryPolicy{
			Backoff:     []string{"5s", "10s", "15s", "20s", "30s"},
			MaxAttempts: int32(min(r.opt.maxAttempts, math.MaxInt32)), //nolint:gosec // bounded just above
		},
		"retention_days":           7,
		"suppression_enabled":      true,
		"unsubscribe_mode":         "sendplane",
		"unsubscribe_url_template": "https://app.load.test/u?e={{ recipient.email | url_encode }}",
		"default_locale":           "en",
		"tracking": trackingConfig{
			Domain: r.opt.trackingDomain,
			Opens:  &yes,
			Clicks: &yes,
			SigningKeys: []signingKeyInfo{{
				Kid:    "load",
				Secret: base64.StdEncoding.EncodeToString([]byte("sendplane-load-tracking-key-0123")),
			}},
		},
	}
	var settings tenantSettings
	if err := r.api.putJSON(ctx, "/api/v1/settings", update, &settings); err != nil {
		return fmt.Errorf("update settings: %w", err)
	}
	if settings.Tracking == nil || settings.Tracking.Domain != r.opt.trackingDomain {
		return fmt.Errorf("tracking domain did not stick: %+v", settings.Tracking)
	}
	r.logf("tenant settings: tracking on (%s), unsubscribe_mode=%s, suppression on, max_attempts=%d",
		settings.Tracking.Domain, settings.UnsubscribeMode, r.opt.maxAttempts)

	var transport idOnly
	if err := r.api.postJSON(ctx, "/api/v1/transports", transportInput{
		Name:     "chaos",
		Host:     "chaos-smtp",
		Port:     2525,
		TLS:      "none",
		MaxConns: 64,
	}, &transport); err != nil {
		return fmt.Errorf("create transport: %w", err)
	}

	var snd idOnly
	if err := r.api.postJSON(ctx, "/api/v1/senders", senderInput{
		Name:        "load",
		FromName:    "sendplane load",
		FromEmail:   "load@" + r.opt.domain,
		TransportID: transport.ID,
	}, &snd); err != nil {
		return fmt.Errorf("create sender: %w", err)
	}

	var tpl idOnly
	if err := r.api.postJSON(ctx, "/api/v1/templates", templateInput{
		Name:          "load",
		Subject:       "sendplane load test",
		Mode:          "mjml",
		Body:          loadMJML,
		DefaultLocale: "en",
	}, &tpl); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	var version messageVersion
	if err := r.api.postJSON(ctx, "/api/v1/templates/"+tpl.ID+"/publish", map[string]any{}, &version); err != nil {
		return fmt.Errorf("publish template: %w", err)
	}
	r.versionID, r.links = version.ID, version.Links
	if len(version.Links) == 0 {
		return errors.New("the published version has no trackable link; the 1M run would not exercise link rewriting")
	}
	r.logf("published version %s with %d trackable link(s): %v", version.ID, len(version.Links), version.Links)

	var camp campaign
	if err := r.api.postJSON(ctx, "/api/v1/campaigns", campaignInput{
		Name:      fmt.Sprintf("load-%d", r.opt.recipients),
		VersionID: version.ID,
		SenderID:  snd.ID,
	}, &camp); err != nil {
		return fmt.Errorf("create campaign: %w", err)
	}
	r.campaignID = camp.ID
	r.rep.CampaignID = camp.ID
	r.logf("campaign %s created", camp.ID)
	return nil
}

// loadMJML is the smallest template that still exercises the two per-recipient
// paths the load test cares about: a Liquid substitution and a link that the
// sender has to rewrite into a signed click URL.
const loadMJML = `<mjml><mj-body><mj-section><mj-column>
<mj-text>Hello {{ recipient.name }}, this is a sendplane load test.</mj-text>
<mj-text><a href="https://app.load.test/offer">See the offer</a></mj-text>
<mj-text><a href="{{ unsubscribe_url }}">Unsubscribe</a></mj-text>
</mj-column></mj-section></mj-body></mjml>`

// writeChunk streams recipients [from,to) as NDJSON.
func (r *runner) writeChunk(w io.Writer, from, to int) error {
	bw := bufio.NewWriterSize(w, 256<<10)
	for i := from; i < to; i++ {
		if _, err := fmt.Fprintf(bw, "{\"email\":%q,\"name\":\"Load User %d\"}\n", r.email(i), i); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func (r *runner) ingest(ctx context.Context) error {
	path := "/api/v1/campaigns/" + r.campaignID + "/recipients"
	chunks := (r.opt.recipients + r.opt.chunk - 1) / r.opt.chunk

	start := time.Now()
	var lastTotal int64
	for c := range chunks {
		from := c * r.opt.chunk
		to := min(from+r.opt.chunk, r.opt.recipients)
		var res ingestResult
		if err := r.api.postNDJSON(ctx, path, fmt.Sprintf("chunk-%04d", c),
			func(w io.Writer) error { return r.writeChunk(w, from, to) }, &res); err != nil {
			return fmt.Errorf("chunk %d: %w", c, err)
		}
		if got, want := res.Accepted, int64(to-from); got != want {
			r.fail("chunk %d accepted %d recipients, want %d (duplicates=%d invalid=%d)",
				c, got, want, res.Duplicates, res.Invalid)
		}
		lastTotal = res.Total
		elapsed := time.Since(start)
		r.logf("chunk %d/%d: accepted=%d total=%d (%.0f rows/s)",
			c+1, chunks, res.Accepted, res.Total, float64(to)/elapsed.Seconds())
	}
	ingestFor := time.Since(start)
	r.rep.IngestSeconds = ingestFor.Seconds()
	r.rep.IngestRowsPerSecond = float64(r.opt.recipients) / ingestFor.Seconds()
	r.logf("ingest of %d recipients in %s (%.0f rows/s), campaign total=%d",
		r.opt.recipients, ingestFor.Round(time.Millisecond), r.rep.IngestRowsPerSecond, lastTotal)

	if lastTotal != int64(r.opt.recipients) {
		r.fail("campaign holds %d recipients after ingest, want %d", lastTotal, r.opt.recipients)
	}
	if ingestFor > r.opt.ingestBudget {
		r.fail("ingest took %s, over the %s budget", ingestFor.Round(time.Millisecond), r.opt.ingestBudget)
	}

	return r.replayChunk(ctx, path, chunks)
}

// replayChunk re-sends one chunk twice, because the two ways of repeating a
// chunk are two different guarantees (architecture 7.2):
//
//   - same Idempotency-Key: the stored result comes back and nothing is
//     inserted, so accepted still reads as the original chunk size and
//     idempotent_replay is true;
//   - a fresh key with the same body: the chunk is really re-ingested and the
//     per-campaign unique index turns every line into a duplicate, so
//     accepted is 0 and duplicates is the chunk size.
func (r *runner) replayChunk(ctx context.Context, path string, chunks int) error {
	c := min(r.opt.replayChunk, chunks-1)
	from := c * r.opt.chunk
	to := min(from+r.opt.chunk, r.opt.recipients)
	size := int64(to - from)
	write := func(w io.Writer) error { return r.writeChunk(w, from, to) }

	start := time.Now()
	var same ingestResult
	if err := r.api.postNDJSON(ctx, path, fmt.Sprintf("chunk-%04d", c), write, &same); err != nil {
		return fmt.Errorf("replay chunk %d with the same key: %w", c, err)
	}
	if !same.IdempotentReplay {
		r.fail("replaying chunk %d with the same Idempotency-Key was not reported as an idempotent replay", c)
	}
	if same.Accepted != size || same.Total != int64(r.opt.recipients) {
		r.fail("same-key replay of chunk %d returned accepted=%d total=%d, want the stored %d and %d",
			c, same.Accepted, same.Total, size, r.opt.recipients)
	}

	var fresh ingestResult
	if err := r.api.postNDJSON(ctx, path, fmt.Sprintf("chunk-%04d-replay", c), write, &fresh); err != nil {
		return fmt.Errorf("replay chunk %d with a fresh key: %w", c, err)
	}
	if fresh.Accepted != 0 || fresh.Duplicates != size {
		r.fail("fresh-key replay of chunk %d returned accepted=%d duplicates=%d, want 0 and %d",
			c, fresh.Accepted, fresh.Duplicates, size)
	}
	if fresh.Total != int64(r.opt.recipients) {
		r.fail("fresh-key replay of chunk %d left the campaign at %d recipients, want %d",
			c, fresh.Total, r.opt.recipients)
	}
	r.rep.ReplaySeconds = time.Since(start).Seconds()
	r.logf("chunk %d replayed twice in %s: same key -> accepted=%d idempotent_replay=%v, fresh key -> accepted=%d duplicates=%d",
		c, time.Since(start).Round(time.Millisecond), same.Accepted, same.IdempotentReplay, fresh.Accepted, fresh.Duplicates)
	return nil
}

// stallWarnAfter is how long the run tolerates "everything is still pending"
// before it says why that happens, since the symptom (a campaign that never
// moves) is a long way from the cause.
const stallWarnAfter = 60 * time.Second

// pollUntilDone polls the campaign until the control finalizer marks it
// completed, printing the cached counts as they move (architecture 7.3: the
// aggregate is recomputed by the leader, so it lags by a few seconds and, past
// 100k rows, by up to a minute).
func (r *runner) pollUntilDone(ctx context.Context) error {
	start := time.Now()
	killed := false
	warnedStall := false
	movedAt := start
	var last campaign
	var prevDone int64
	prevAt := start

	for {
		var c campaign
		if err := r.api.getJSON(ctx, "/api/v1/campaigns/"+r.campaignID, &c); err != nil {
			r.logf("poll failed (retrying): %v", err)
		} else {
			last = c
			done := c.count("sent") + c.count("failed") + c.count("suppressed")
			now := time.Now()
			rate := float64(done-prevDone) / now.Sub(prevAt).Seconds()
			prevDone, prevAt = done, now
			r.logf("status=%s sent=%d failed=%d suppressed=%d in-flight=%d (%.0f msg/s, %.1f%%)",
				c.Status, c.count("sent"), c.count("failed"), c.count("suppressed"),
				c.inFlight(), rate, 100*float64(done)/float64(r.opt.recipients))

			if !killed && r.opt.killSender && float64(done) >= r.opt.killAt*float64(r.opt.recipients) {
				killed = true
				if err := r.killAndRestartSender(ctx); err != nil {
					// A failed kill is worth an assertion, not an aborted run:
					// everything else the run measures is still valid.
					r.fail("the sender kill/restart step failed: %v", err)
				}
			}
			if done > 0 || c.count("pending") == 0 {
				movedAt = now
			} else if now.Sub(movedAt) > stallWarnAfter && !warnedStall {
				warnedStall = true
				r.logf("WARNING: %d deliveries have been pending for %s with nothing sent. "+
					"Senders only claim queued and deferred rows (store/doc.go), so a campaign "+
					"whose rows are still pending will never move.",
					c.count("pending"), stallWarnAfter)
			}
			if c.Status == "completed" && c.inFlight() == 0 {
				r.rep.RunSeconds = time.Since(start).Seconds()
				r.rep.ThroughputPerSecond = float64(r.opt.recipients) / r.rep.RunSeconds
				r.logf("campaign completed in %s (%.0f msg/s end to end)",
					time.Since(start).Round(time.Millisecond), r.rep.ThroughputPerSecond)
				return nil
			}
			if c.Status == "cancelled" || c.Status == "paused" {
				return fmt.Errorf("campaign ended up %s", c.Status)
			}
		}

		select {
		case <-ctx.Done():
			r.rep.RunSeconds = time.Since(start).Seconds()
			return fmt.Errorf("budget %s exhausted with the campaign still %s (sent=%d failed=%d in-flight=%d)",
				r.opt.budget, last.Status, last.count("sent"), last.count("failed"), last.inFlight())
		case <-time.After(r.opt.poll):
		}
	}
}

// killAndRestartSender SIGKILLs one sender replica and brings it back, so the
// run covers the lease recovery path of architecture 15.1. The dead replica's
// rows stay leased until they expire (sender.lease_for in config.yaml) and the
// control lease reaper releases them.
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

	r.logf("killing sender container %s with SIGKILL", victim[:min(12, len(victim))])
	//nolint:gosec // G204: the docker command and the container name are this
	// generator's own flags and its own compose project, not request input.
	if out, err := exec.CommandContext(ctx, r.opt.dockerCmd, "kill", "--signal=KILL", victim).CombinedOutput(); err != nil {
		return fmt.Errorf("docker kill: %w: %s", err, strings.TrimSpace(string(out)))
	}
	r.rep.SenderKilled = true

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.opt.killDowntime):
	}

	r.logf("restarting the killed sender replica")
	if _, err := r.composeOutput(ctx, "up", "-d", "--no-recreate", "sender"); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}
	r.rep.SenderRestarted = true
	return nil
}

// composeOutput shells out to Compose. The command is a flag because the
// plugin form ("docker compose") and the standalone binary ("docker-compose")
// are both in the wild and the generator has no way to guess which one is
// installed.
func (r *runner) composeOutput(ctx context.Context, args ...string) (string, error) {
	argv := strings.Fields(r.opt.composeCmd)
	if len(argv) == 0 {
		return "", errors.New("--compose-cmd is empty")
	}
	full := make([]string, 0, len(argv)+2+len(args))
	full = append(full, argv[1:]...)
	full = append(full, "-f", r.opt.composeFile)
	full = append(full, args...)
	//nolint:gosec // G204: argv and the compose file come from this
	// generator's own flags.
	out, err := exec.CommandContext(ctx, argv[0], full...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", argv[0], strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (r *runner) assert(ctx context.Context) error {
	var c campaign
	if err := r.api.getJSON(ctx, "/api/v1/campaigns/"+r.campaignID, &c); err != nil {
		return fmt.Errorf("read the final campaign: %w", err)
	}
	sent, failed, suppressed := c.count("sent"), c.count("failed"), c.count("suppressed")
	r.rep.ByStatus = map[string]int64{}
	if c.Stats != nil {
		r.rep.ByStatus = c.Stats.ByStatus
	}
	r.rep.Sent, r.rep.Failed, r.rep.Suppressed = sent, failed, suppressed

	if c.Status != "completed" {
		r.fail("campaign is %s, want completed", c.Status)
	}
	if total := sent + failed + suppressed; total != int64(r.opt.recipients) {
		r.fail("sent+failed+suppressed = %d, want %d", total, r.opt.recipients)
	}
	for _, s := range []string{"pending", "queued", "leased", "deferred"} {
		if n := c.count(s); n != 0 {
			r.fail("%d deliveries are still %s", n, s)
		}
	}
	// Exact, not approximate: the decision function is deterministic in
	// (seed, recipient, attempt), so even the duplicate sends a SIGKILL causes
	// replay the same decision and cannot move these two numbers.
	if sent != r.exp.Sent {
		r.fail("sent = %d, want %d (derived from chaossmtp.Decide)", sent, r.exp.Sent)
	}
	if failed != r.exp.Failed {
		r.fail("failed = %d, want %d (derived from chaossmtp.Decide)", failed, r.exp.Failed)
	}

	r.assertChaos(ctx, sent)
	r.assertFailedSample(ctx)
	r.assertLinks(ctx)
	r.sampleLatency(ctx)
	r.measureDB(ctx)
	return nil
}

// assertChaos compares what the relay saw with what the campaign recorded. An
// accepted message the campaign does not count as sent is a real double send:
// chaossmtp decides before it records, so a dropped connection is never
// recorded and cannot inflate Accepted (internal/chaossmtp/README.md).
func (r *runner) assertChaos(ctx context.Context, sent int64) {
	st, err := fetchChaosStats(ctx, r.hc, r.opt.chaosStats)
	if err != nil {
		r.fail("reading chaos-smtp /stats failed: %v", err)
		return
	}
	r.rep.Chaos = st

	dup := st.Accepted - sent
	r.rep.DuplicatesFromRecovery = dup
	switch {
	case dup < 0:
		r.fail("chaos-smtp accepted %d messages but the campaign counts %d sent; the relay saw fewer sends than were recorded",
			st.Accepted, sent)
	case dup == 0:
		// Exactly once.
	case !r.opt.killSender:
		r.fail("chaos-smtp accepted %d messages for %d sent deliveries (%d duplicates) without a sender kill",
			st.Accepted, sent, dup)
	default:
		// A replica SIGKILLed between "the relay took it" and "the row says
		// sent" re-sends that message after its lease expires. It is
		// at-least-once by design, but only just: architecture 15.1 wants the
		// recovered duplicates reported and kept marginal.
		limit := float64(r.opt.recipients) * 0.0001
		if float64(dup) > limit {
			r.fail("%d duplicate sends recovered after the SIGKILL, over the %.0f allowed (0.01%% of %d)",
				dup, limit, r.opt.recipients)
		} else {
			r.logf("duplicates_from_recovery = %d (%.4f%% of %d, within the 0.01%% allowance)",
				dup, 100*float64(dup)/float64(r.opt.recipients), r.opt.recipients)
		}
	}

	// The attempt-level counters are exact for a clean run. With a kill they
	// drift by the same handful of replayed attempts, so they get the same
	// allowance as the duplicate check.
	r.assertChaosCounter("tempfail replies", st.TempFailed, r.exp.TempFails)
	r.assertChaosCounter("permfail replies", st.PermFailed, r.exp.PermFails)
	r.assertChaosCounter("dropped connections", st.Dropped, r.exp.Drops)
}

func (r *runner) assertChaosCounter(name string, got, want int64) {
	if got == want {
		return
	}
	if r.opt.killSender {
		limit := int64(float64(r.opt.recipients)*0.0001) + 1
		if got >= want && got-want <= limit {
			r.logf("chaos-smtp %s = %d, %d over the derived %d (replayed attempts after the SIGKILL)",
				name, got, got-want, want)
			return
		}
	}
	r.fail("chaos-smtp %s = %d, want %d", name, got, want)
}

// assertFailedSample checks that no delivery failed for a reason other than
// the two the policy allows: a permanent reply, or a transient one that ran
// out of attempts (architecture 15.1).
func (r *runner) assertFailedSample(ctx context.Context) {
	var list deliveryList
	path := fmt.Sprintf("/api/v1/campaigns/%s/deliveries?status=failed&limit=%d", r.campaignID, r.opt.sample)
	if err := r.api.getJSON(ctx, path, &list); err != nil {
		r.fail("sampling failed deliveries: %v", err)
		return
	}
	r.rep.FailedSampleSize = len(list.Items)
	if len(list.Items) == 0 && r.exp.Failed > 0 {
		r.fail("no failed delivery came back although %d are expected", r.exp.Failed)
		return
	}
	bad := 0
	for _, d := range list.Items {
		permanent := d.LastErrorClass == "permanent" || d.LastErrorClass == "policy"
		exhausted := int(d.AttemptCount) >= r.opt.maxAttempts
		if !permanent && !exhausted {
			bad++
			if bad <= 5 {
				r.fail("delivery %s failed with attempt_count=%d and last_error_class=%q: neither exhausted nor permanent",
					d.ID, d.AttemptCount, d.LastErrorClass)
			}
		}
	}
	if bad > 5 {
		r.fail("...and %d more failed deliveries that are neither exhausted nor permanent", bad-5)
	}
	r.logf("failed-delivery sample: %d rows, %d inconsistent", len(list.Items), bad)
}

// assertLinks confirms the campaign really published a trackable link, which
// is what makes the run exercise per-recipient link rewriting.
func (r *runner) assertLinks(ctx context.Context) {
	var links linkClickList
	if err := r.api.getJSON(ctx, "/api/v1/campaigns/"+r.campaignID+"/links", &links); err != nil {
		r.fail("reading campaign links: %v", err)
		return
	}
	r.rep.TrackedLinks = len(links.Items)
	if len(links.Items) == 0 {
		r.fail("the campaign reports no trackable link, so no delivery went through the click rewrite")
	}
}

// sampleLatency measures created_at -> sent_at on randomly chosen recipients.
// It is the queue latency of architecture 15.1's "claim -> sent" p95 seen from
// the outside: created_at is the ingest time, so for a campaign this size the
// number is dominated by how long a row waits, which is exactly the trend the
// artifact is for.
func (r *runner) sampleLatency(ctx context.Context) {
	rnd := rand.New(rand.NewPCG(r.opt.seed, uint64(max(r.opt.recipients, 0)))) //nolint:gosec // non-negative
	var ms []float64
	for range r.opt.sample {
		i := rnd.IntN(r.opt.recipients)
		var list deliveryList
		path := fmt.Sprintf("/api/v1/campaigns/%s/deliveries?email=%s&limit=1",
			r.campaignID, queryEscape(r.email(i)))
		if err := r.api.getJSON(ctx, path, &list); err != nil || len(list.Items) == 0 {
			continue
		}
		d := list.Items[0]
		if d.Status != "sent" || d.SentAt == nil || d.CreatedAt == nil {
			continue
		}
		ms = append(ms, float64(d.SentAt.Sub(*d.CreatedAt).Milliseconds()))
	}
	if len(ms) == 0 {
		r.logf("latency sample: no sent delivery sampled")
		return
	}
	sort.Float64s(ms)
	r.rep.LatencySampleSize = len(ms)
	r.rep.P50CreatedToSentMs = percentile(ms, 0.50)
	r.rep.P95CreatedToSentMs = percentile(ms, 0.95)
	r.logf("created->sent over %d sampled deliveries: p50=%.0fms p95=%.0fms",
		len(ms), r.rep.P50CreatedToSentMs, r.rep.P95CreatedToSentMs)
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func (r *runner) measureDB(ctx context.Context) {
	if r.opt.dsn == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, r.opt.dsn)
	if err != nil {
		r.logf("database size unavailable: %v", err)
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var size int64
	if err := conn.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&size); err != nil {
		r.logf("database size unavailable: %v", err)
		return
	}
	r.rep.DBSizeBytes = size
	var attempts int64
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM delivery_attempt").Scan(&attempts); err == nil {
		r.rep.AttemptRows = attempts
	}
	r.logf("database size %.1f MiB, %d delivery_attempts rows (expected %d for a clean run)",
		float64(size)/(1<<20), r.rep.AttemptRows, r.exp.Attempts)
}
