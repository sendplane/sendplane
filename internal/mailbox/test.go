package mailbox

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/store"
)

// Test connects to a mailbox the way the poller and the probe collector do -
// dial, negotiate TLS, log in, look at the configured folders, log out again -
// and reports how far it got.
//
// A remote failure is never a Go error: "the password is wrong" is the answer
// to the question, not a failure to answer it. Only a caller mistake (a config
// that cannot be dialed at all, a password that will not decrypt) shows up as
// OK=false with Stage=StageConfig, and even that is data rather than an error
// so that a handler has one shape to render.
//
// It is what POST /probe-mailboxes/test answers with and what the mailbox-check
// leader loop records as store.MailboxHealth.
func Test(ctx context.Context, cfg Config, cipher host.SecretCipher) TestResult {
	start := time.Now()
	res := TestResult{Folders: map[string]FolderInfo{}}

	if err := cfg.Validate(); err != nil {
		return res.fail(StageConfig, err, start)
	}
	cfg = cfg.withDefaults()
	// One bounded attempt: a test is a foreground request, and an operator
	// waiting on a form learns more from "timed out after 20s" than from a
	// connection that is still being retried.
	ctx, cancel := context.WithTimeout(ctx, TestTimeout)
	defer cancel()
	if cfg.DialTimeout > TestTimeout {
		cfg.DialTimeout = TestTimeout
	}
	if cfg.Timeout > TestTimeout {
		cfg.Timeout = TestTimeout
	}

	password := cfg.Password
	if cipher != nil && len(password) > 0 {
		plain, err := cipher.Decrypt(ctx, password)
		if err != nil {
			return res.fail(StageConfig, fmt.Errorf("cannot decrypt the stored password: %w", err), start)
		}
		password = plain
	}

	if cfg.Protocol == ProtocolIMAP {
		return testIMAP(ctx, cfg, string(password), res, start)
	}
	return testPOP3(ctx, cfg, string(password), res, start)
}

// TestTimeout bounds one whole Test call, every stage included.
const TestTimeout = 20 * time.Second

// The stages a Test reports. They are in the order they are reached, and the
// one in a TestResult is the first that failed - or StageOK.
const (
	// StageConfig is a mailbox that cannot be dialed at all: an unknown
	// protocol, no host, a password that will not decrypt. Nothing was sent.
	StageConfig = "config"
	StageDial   = store.MailboxStageDial
	StageTLS    = store.MailboxStageTLS
	StageAuth   = store.MailboxStageAuth
	StageFolder = store.MailboxStageFolder
	StageOK     = store.MailboxStageOK
)

// DialError is a Dial failure carrying the stage it happened at, so that a
// caller which only ever gets an error out of Dial - the bounce poller, the
// probe collector - can still record the same store.MailboxHealth a Test
// would, instead of filing every failure as "unreachable".
type DialError struct {
	Stage string
	Err   error
}

func (e *DialError) Error() string { return e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

// MailboxStage is Stage behind a method, so that a package which must not
// import this one can still read it through a one-method interface.
// internal/probe is the case: it takes its mailbox client from the root
// adapter and never sees this package's types.
func (e *DialError) MailboxStage() string { return e.Stage }

func stageErr(stage string, err error) error { return &DialError{Stage: stage, Err: err} }

// StageOf reports the stage a mailbox error happened at. An error that carries
// none - anything that went wrong after the connection was established - is
// reported as StageDial, which is what a caller writing health wants: the
// session did not work.
func StageOf(err error) string {
	var de *DialError
	if errors.As(err, &de) && de.Stage != "" {
		return de.Stage
	}
	return StageDial
}

// loginStage classifies a failed LOGIN / PASS: a tagged NO/BAD or a -ERR is
// the server rejecting the credentials, anything else broke the connection
// while they were in flight.
func loginStage(err error) string {
	if refused(err) {
		return StageAuth
	}
	return StageDial
}

// FolderInfo is what Test saw of one folder.
type FolderInfo struct {
	// Exists is false for a folder the server does not have, which is the
	// usual shape of a mistyped spam folder.
	Exists bool
	// Messages is the message count the server reported (IMAP EXISTS, POP3
	// STAT). It is a sanity check for an operator: a probe mailbox that has
	// been collecting thousands of messages is not being drained.
	Messages int
}

// TestResult is one credential check.
type TestResult struct {
	OK bool
	// Stage is how far the check got: config, dial, tls, auth, folder or ok.
	Stage string
	// Error is the failure text, empty when OK. It is the server's own
	// wording where there is one, because "Invalid credentials (Failure)" is
	// what an operator will search the provider's help for.
	Error string
	// Latency is how long the whole check took.
	Latency time.Duration
	// Folders is keyed by folder name, in the mailbox's own naming.
	Folders map[string]FolderInfo
	// Server summarizes what answered: the POP3 greeting, or the IMAP
	// capabilities. It is diagnostic only.
	Server string
}

func (r TestResult) fail(stage string, err error, start time.Time) TestResult {
	r.OK = false
	r.Stage = stage
	r.Error = cleanErr(err)
	r.Latency = time.Since(start)
	return r
}

func (r TestResult) succeed(start time.Time) TestResult {
	r.OK = true
	r.Stage = StageOK
	r.Latency = time.Since(start)
	return r
}

// cleanErr renders an error for an operator: the package prefix this package
// puts on everything is noise in a form's error line, and a server's own
// wording is the useful part.
func cleanErr(err error) string {
	if err == nil {
		return ""
	}
	var se *ServerError
	if errors.As(err, &se) && se.Text != "" {
		return se.Text
	}
	var ie *imap.Error
	if errors.As(err, &ie) {
		return ie.Error()
	}
	return strings.TrimPrefix(err.Error(), "mailbox: ")
}

// refused reports whether err is the server rejecting a command rather than
// the connection failing. It is what separates auth from dial.
func refused(err error) bool {
	var se *ServerError
	if errors.As(err, &se) {
		return true
	}
	var ie *imap.Error
	return errors.As(err, &ie)
}

// testFolders is the folder list Test inspects: the configured folder plus
// ExtraFolders, without repeats.
func (c Config) testFolders() []string {
	out := []string{c.Folder}
	for _, f := range c.ExtraFolders {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		dup := false
		for _, have := range out {
			if strings.EqualFold(have, f) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, f)
		}
	}
	return out
}

// --- IMAP --------------------------------------------------------------

func testIMAP(ctx context.Context, cfg Config, password string, res TestResult, start time.Time) TestResult {
	conn, stage, err := testDial(ctx, cfg)
	if err != nil {
		return res.fail(stage, err, start)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline(ctx, cfg.Timeout))

	var c *imapclient.Client
	opts := &imapclient.Options{TLSConfig: cfg.tlsConfig()}
	if cfg.TLS == store.TLSSTARTTLS {
		c, err = imapclient.NewStartTLS(conn, opts)
		if err != nil {
			return res.fail(StageTLS, err, start)
		}
	} else {
		c = imapclient.New(conn, opts)
		if err := c.WaitGreeting(); err != nil {
			return res.fail(StageDial, err, start)
		}
	}
	defer func() { _ = c.Close() }()
	res.Server = capsSummary(c.Caps())

	if err := c.Login(cfg.Username, password).Wait(); err != nil {
		// A tagged NO/BAD is the server saying no; anything else broke the
		// connection while the credentials were in flight.
		stage := StageDial
		if refused(err) {
			stage = StageAuth
		}
		return res.fail(stage, err, start)
	}

	var firstErr error
	for _, folder := range cfg.testFolders() {
		// EXAMINE, not SELECT: a credential check must not clear \Recent or
		// otherwise disturb a mailbox the poller is about to read.
		data, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			res.Folders[folder] = FolderInfo{}
			if firstErr == nil {
				firstErr = fmt.Errorf("folder %q: %w", folder, err)
			}
			continue
		}
		res.Folders[folder] = FolderInfo{Exists: true, Messages: int(data.NumMessages)}
	}
	if firstErr != nil {
		return res.fail(StageFolder, firstErr, start)
	}

	if err := c.Logout().Wait(); err != nil {
		// The credentials worked and the folders are there; a refused logout
		// says nothing about either.
		_ = err
	}
	return res.succeed(start)
}

// capsSummary renders the server's capabilities as one sorted line.
func capsSummary(caps imap.CapSet) string {
	if len(caps) == 0 {
		return ""
	}
	out := make([]string, 0, len(caps))
	for c := range caps {
		out = append(out, string(c))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// --- POP3 --------------------------------------------------------------

func testPOP3(ctx context.Context, cfg Config, password string, res TestResult, start time.Time) TestResult {
	conn, stage, err := testDial(ctx, cfg)
	if err != nil {
		return res.fail(stage, err, start)
	}
	m := &pop3Mailbox{cfg: cfg, conn: conn}
	m.reset(conn)
	defer func() { _ = m.conn.Close() }()
	_ = conn.SetDeadline(deadline(ctx, cfg.Timeout))

	greeting, err := m.readStatus()
	if err != nil {
		return res.fail(StageDial, err, start)
	}
	res.Server = greeting

	if cfg.TLS == store.TLSSTARTTLS {
		if _, err := m.cmd("STLS"); err != nil {
			return res.fail(StageTLS, err, start)
		}
		tc := tls.Client(conn, cfg.tlsConfig())
		if err := tc.HandshakeContext(ctx); err != nil {
			return res.fail(StageTLS, err, start)
		}
		m.conn = tc
		m.reset(tc)
		_ = tc.SetDeadline(deadline(ctx, cfg.Timeout))
	}

	if _, err := m.cmd("USER " + cfg.Username); err != nil {
		stage := StageDial
		if refused(err) {
			stage = StageAuth
		}
		return res.fail(stage, err, start)
	}
	if _, err := m.cmd("PASS " + password); err != nil {
		stage := StageDial
		if refused(err) {
			stage = StageAuth
		}
		return res.fail(stage, err, start)
	}

	// POP3 has one implicit folder, so STAT is the whole inspection. It is
	// reported under the configured folder name so that a caller renders both
	// protocols the same way.
	stat, err := m.cmd("STAT")
	if err != nil {
		return res.fail(StageFolder, err, start)
	}
	res.Folders[cfg.Folder] = FolderInfo{Exists: true, Messages: statCount(stat)}

	_, _ = m.cmd("QUIT")
	return res.succeed(start)
}

// statCount reads the message count out of a "+OK 3 1024" STAT response.
func statCount(stat string) int {
	first, _, _ := strings.Cut(strings.TrimSpace(stat), " ")
	n := 0
	for _, r := range first {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// --- shared ------------------------------------------------------------

// testDial opens the connection, telling a TCP failure apart from a TLS
// handshake failure - which cfg.dialConn folds into one error, because the
// poller only ever needs "it did not connect".
func testDial(ctx context.Context, cfg Config) (net.Conn, string, error) {
	dialCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()

	dial := cfg.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(dialCtx, "tcp", cfg.Addr())
	if err != nil {
		return nil, StageDial, err
	}
	if cfg.TLS != store.TLSImplicit {
		return conn, StageDial, nil
	}
	tc := tls.Client(conn, cfg.tlsConfig())
	if err := tc.HandshakeContext(dialCtx); err != nil {
		_ = conn.Close()
		return nil, StageTLS, err
	}
	return tc, StageTLS, nil
}
