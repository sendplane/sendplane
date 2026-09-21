package probe

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

func fixture(t *testing.T, name string) Headers {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return ParseHeaders(raw)
}

func TestParseHeadersUnfolds(t *testing.T) {
	t.Parallel()

	h := ParseHeaders([]byte("Subject: one\r\n two\r\nTo: a@b\r\n\r\nSubject: in the body\r\n"))
	if got := h.Get("subject"); got != "one two" {
		t.Errorf("Subject = %q, want %q", got, "one two")
	}
	if got := h.Get("To"); got != "a@b" {
		t.Errorf("To = %q", got)
	}
	// The body is not the header block, however much it looks like one.
	if n := len(h); n != 2 {
		t.Errorf("headers = %d, want 2: %+v", n, h)
	}
}

func TestParseAuthResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		authServID string
		method     string
		result     string
		props      map[string]string
		policy     string
	}{
		{
			name: "gmail",
			in: "mx.google.com; dkim=pass header.i=@mail.example.com header.s=sp1 header.b=Q1w2; " +
				"spf=pass (google.com: domain of b@bounce.example.com designates 203.0.113.10 as permitted sender) smtp.mailfrom=b@bounce.example.com; " +
				"dmarc=pass (p=REJECT sp=REJECT dis=NONE) header.from=mail.example.com",
			authServID: "mx.google.com",
			method:     "dmarc", result: "pass", policy: "reject",
			props: map[string]string{"header.from": "mail.example.com"},
		},
		{
			name:       "version number after the authserv-id",
			in:         "mx.example.com 1; spf=pass smtp.mailfrom=a@b.test",
			authServID: "mx.example.com",
			method:     "spf", result: "pass",
			props: map[string]string{"smtp.mailfrom": "a@b.test"},
		},
		{
			name:       "exchange omits the authserv-id",
			in:         "spf=pass (sender IP is 203.0.113.11) smtp.mailfrom=bounce.example.com; dkim=pass (signature was verified) header.d=mail.example.com;dmarc=pass action=none header.from=mail.example.com",
			authServID: "",
			method:     "dkim", result: "pass",
			props: map[string]string{"header.d": "mail.example.com"},
		},
		{
			name:       "comment containing a semicolon",
			in:         `mx.example.com; spf=fail (example.com: domain of a@b.test does not designate 1.2.3.4; see spf) smtp.mailfrom=a@b.test`,
			authServID: "mx.example.com",
			method:     "spf", result: "fail",
			props: map[string]string{"smtp.mailfrom": "a@b.test"},
		},
		{
			name:       "reason",
			in:         `mx.example.com; dkim=fail reason="key not found" header.d=mail.example.com`,
			authServID: "mx.example.com",
			method:     "dkim", result: "fail",
			props: map[string]string{"header.d": "mail.example.com"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ar, err := ParseAuthResults(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if ar.AuthServID != tc.authServID {
				t.Errorf("authserv-id = %q, want %q", ar.AuthServID, tc.authServID)
			}
			m, ok := ar.Method(tc.method)
			if !ok {
				t.Fatalf("no %s method in %+v", tc.method, ar.Methods)
			}
			if m.Result != tc.result {
				t.Errorf("%s = %q, want %q", tc.method, m.Result, tc.result)
			}
			for k, want := range tc.props {
				if got := m.Prop(k); got != want {
					t.Errorf("%s.%s = %q, want %q", tc.method, k, got, want)
				}
			}
			if tc.policy != "" {
				if got := DMARCPolicy(m); got != tc.policy {
					t.Errorf("policy = %q, want %q", got, tc.policy)
				}
			}
		})
	}

	t.Run("reason is not a property", func(t *testing.T) {
		t.Parallel()
		ar, err := ParseAuthResults(`mx.example.com; dkim=fail reason="key not found" header.d=x.test`)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := ar.Method("dkim")
		if m.Reason != "key not found" {
			t.Errorf("reason = %q", m.Reason)
		}
	})
}

func TestTrustedAuthResults(t *testing.T) {
	t.Parallel()

	h := Headers{
		{"Authentication-Results", "mx.attacker.test; dkim=pass header.d=mail.example.com"},
		{"Authentication-Results", "mx.google.com; dkim=fail header.d=mail.example.com"},
	}
	got := TrustedAuthResults(h, "mx.google.com")
	if len(got) != 1 {
		t.Fatalf("trusted = %d headers, want 1", len(got))
	}
	// A header anyone upstream can add must not be able to turn a fail into a
	// pass (ADR-0012).
	m, _ := got[0].Method("dkim")
	if m.Result != "fail" {
		t.Fatalf("dkim = %q, want fail", m.Result)
	}

	if n := len(TrustedAuthResults(h, "")); n != 2 {
		t.Fatalf("unconfigured authserv-id = %d headers, want all 2", n)
	}
}

func TestFirstExternalHop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		file string
		ip   string
		rdns string
		tls  bool
	}{
		{file: "gmail_pass.eml", ip: "203.0.113.10", rdns: "smtp-out1.example.com", tls: true},
		{file: "gmail_spam_dkimfail.eml", ip: "203.0.113.10", rdns: "smtp-out1.example.com", tls: true},
		{file: "outlook_pass.eml", ip: "203.0.113.11", tls: true},
		{file: "postfix_opendmarc.eml", ip: "203.0.113.12", tls: false},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			hop, ok := FirstExternalHop(fixture(t, tc.file))
			if !ok {
				t.Fatal("no external hop found")
			}
			if hop.IP.String() != tc.ip {
				t.Errorf("ip = %s, want %s (%q)", hop.IP, tc.ip, hop.Raw)
			}
			if hop.RDNS != tc.rdns {
				t.Errorf("rdns = %q, want %q", hop.RDNS, tc.rdns)
			}
			if hop.TLS != tc.tls {
				t.Errorf("tls = %v, want %v (%q)", hop.TLS, tc.tls, hop.Raw)
			}
		})
	}
}

func TestParseReceivedInternalHopsAreSkipped(t *testing.T) {
	t.Parallel()

	// The topmost Received of the Postfix sample is a localhost handoff; the
	// hop that matters is the one below it.
	hops := ReceivedChain(fixture(t, "postfix_opendmarc.eml"))
	if len(hops) != 2 {
		t.Fatalf("hops = %d, want 2", len(hops))
	}
	if hops[1].IP.String() != "127.0.0.1" {
		t.Fatalf("newest hop ip = %s, want the loopback handoff", hops[1].IP)
	}
	if hops[0].RDNS != "" {
		t.Errorf("rdns = %q, want empty for Postfix's \"unknown\"", hops[0].RDNS)
	}
}

func TestLatency(t *testing.T) {
	t.Parallel()

	h := fixture(t, "gmail_pass.eml")
	got, ok := Latency(h, time.Date(2026, 9, 21, 10, 14, 35, 0, time.UTC))
	if !ok {
		t.Fatal("no latency")
	}
	// Date is 10:14:05 UTC, the mailbox holds it at 10:14:35.
	if got != 30*time.Second {
		t.Fatalf("latency = %s, want 30s", got)
	}

	t.Run("falls back to the newest Received", func(t *testing.T) {
		t.Parallel()
		got, ok := Latency(h, time.Time{})
		if !ok {
			t.Fatal("no latency")
		}
		// The topmost Received is 03:14:20 -0700 = 10:14:20 UTC.
		if got != 15*time.Second {
			t.Fatalf("latency = %s, want 15s", got)
		}
	})

	t.Run("clock skew never produces a negative latency", func(t *testing.T) {
		t.Parallel()
		got, ok := Latency(h, time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC))
		if !ok || got != 0 {
			t.Fatalf("latency = %s, want 0", got)
		}
	})
}

func TestObserveFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		box     store.ProbeMailbox
		folder  string
		want    store.HealthStatus
		spf     string
		dkim    string
		dmarc   string
		policy  string
		selectr string
	}{
		{
			name:   "gmail pass",
			file:   "gmail_pass.eml",
			box:    store.ProbeMailbox{AuthServID: "mx.google.com", InboxFolder: "INBOX", SpamFolder: "[Gmail]/Spam"},
			folder: "INBOX", want: store.HealthGreen,
			spf: "pass", dkim: "pass", dmarc: "pass", policy: "reject", selectr: "sp1",
		},
		{
			name:   "gmail dkim fail in spam",
			file:   "gmail_spam_dkimfail.eml",
			box:    store.ProbeMailbox{AuthServID: "mx.google.com", InboxFolder: "INBOX", SpamFolder: "[Gmail]/Spam"},
			folder: "[Gmail]/Spam", want: store.HealthRed,
			spf: "pass", dkim: "fail", dmarc: "fail", policy: "none", selectr: "sp1",
		},
		{
			name:   "outlook pass, no authserv-id configured",
			file:   "outlook_pass.eml",
			box:    store.ProbeMailbox{InboxFolder: "Inbox", SpamFolder: "Junk Email"},
			folder: "Inbox", want: store.HealthGreen,
			spf: "pass", dkim: "pass", dmarc: "pass",
		},
		{
			name:   "postfix with opendmarc: no dmarc record and no TLS",
			file:   "postfix_opendmarc.eml",
			box:    store.ProbeMailbox{AuthServID: "mail.example.net", InboxFolder: "INBOX", SpamFolder: "Junk"},
			folder: "INBOX", want: store.HealthYellow,
			spf: "pass", dkim: "pass", dmarc: "none", selectr: "sp1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := fixture(t, tc.file)
			obs := Observe(Evidence{
				Mailbox:    &tc.box,
				Headers:    h,
				Folder:     tc.folder,
				ReceivedAt: time.Date(2026, 9, 21, 10, 20, 0, 0, time.UTC),
			}, time.Now())

			if !obs.TrustedAR {
				t.Fatalf("no trusted Authentication-Results")
			}
			if obs.SPF != tc.spf || obs.DKIM != tc.dkim || obs.DMARC != tc.dmarc {
				t.Errorf("spf/dkim/dmarc = %q/%q/%q, want %q/%q/%q",
					obs.SPF, obs.DKIM, obs.DMARC, tc.spf, tc.dkim, tc.dmarc)
			}
			if tc.policy != "" && obs.DMARCPolicy != tc.policy {
				t.Errorf("dmarc policy = %q, want %q", obs.DMARCPolicy, tc.policy)
			}
			if tc.selectr != "" && obs.DKIMSelector != tc.selectr {
				t.Errorf("dkim selector = %q, want %q", obs.DKIMSelector, tc.selectr)
			}
			if got, reason := Verdict(obs); got != tc.want {
				t.Errorf("verdict = %s (%s), want %s", got, reason, tc.want)
			}
		})
	}
}

func TestVerdict(t *testing.T) {
	t.Parallel()

	pass := Observation{
		Delivered: true, TrustedAR: true,
		SPF: "pass", DKIM: "pass", DMARC: "pass", DMARCPolicy: "reject",
		Folder: FolderInbox, TLS: true, PTRChecked: true, PTRMatch: true,
	}
	with := func(f func(*Observation)) Observation {
		o := pass
		f(&o)
		return o
	}

	tests := []struct {
		name string
		obs  Observation
		want store.HealthStatus
	}{
		{"all pass", pass, store.HealthGreen},
		{"not delivered", with(func(o *Observation) { o.Delivered = false }), store.HealthRed},
		{"spf fail", with(func(o *Observation) { o.SPF = "fail" }), store.HealthRed},
		{"dkim fail", with(func(o *Observation) { o.DKIM = "fail" }), store.HealthRed},
		{"dmarc fail", with(func(o *Observation) { o.DMARC = "fail" }), store.HealthRed},
		{"spam folder", with(func(o *Observation) { o.Folder = FolderSpam }), store.HealthYellow},
		{"dmarc p=none", with(func(o *Observation) { o.DMARCPolicy = "none" }), store.HealthYellow},
		{"no tls", with(func(o *Observation) { o.TLS = false }), store.HealthYellow},
		{"ptr mismatch", with(func(o *Observation) { o.PTRMatch = false }), store.HealthYellow},
		{"softfail", with(func(o *Observation) { o.SPF = "softfail" }), store.HealthYellow},
		{"no trusted header", with(func(o *Observation) { o.TrustedAR = false }), store.HealthYellow},
		{"unchecked ptr is not a mismatch", with(func(o *Observation) { o.PTRChecked, o.PTRMatch = false, false }), store.HealthGreen},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reason := Verdict(tc.obs)
			if got != tc.want {
				t.Fatalf("verdict = %s (%s), want %s", got, reason, tc.want)
			}
		})
	}
}
