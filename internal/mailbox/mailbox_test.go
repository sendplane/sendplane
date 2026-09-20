package mailbox

import (
	"context"
	"errors"
	"testing"

	"github.com/sendplane/sendplane/store"
)

func TestParseAction(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   Action
		folder string
		err    bool
	}{
		{in: "", want: ActionKeep},
		{in: "keep", want: ActionKeep},
		{in: "delete", want: ActionDelete},
		{in: "move:Processed", want: "move:Processed", folder: "Processed"},
		{in: "move:", err: true},
		{in: "move:  ", err: true},
		{in: "burn", err: true},
	} {
		got, err := ParseAction(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseAction(%q): want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAction(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAction(%q) = %q, want %q", tc.in, got, tc.want)
		}
		folder, ok := got.MoveFolder()
		if ok != (tc.folder != "") || folder != tc.folder {
			t.Errorf("MoveFolder(%q) = %q,%v, want %q", got, folder, ok, tc.folder)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	base := Config{Protocol: ProtocolIMAP, Host: "mail.example.com"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, cfg := range map[string]Config{
		"no protocol":  {Host: "h"},
		"bad protocol": {Protocol: "smtp", Host: "h"},
		"no host":      {Protocol: ProtocolIMAP},
		"bad tls":      {Protocol: ProtocolIMAP, Host: "h", TLS: "maybe"},
		"bad action":   {Protocol: ProtocolIMAP, Host: "h", AfterProcess: "shred"},
		"pop3 move":    {Protocol: ProtocolPOP3, Host: "h", AfterProcess: "move:Done"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	for _, tc := range []struct {
		proto Protocol
		mode  store.TLSMode
		port  int
	}{
		{ProtocolIMAP, "", 993},
		{ProtocolIMAP, store.TLSSTARTTLS, 143},
		{ProtocolIMAP, store.TLSNone, 143},
		{ProtocolPOP3, "", 995},
		{ProtocolPOP3, store.TLSSTARTTLS, 110},
	} {
		got := Config{Protocol: tc.proto, Host: "h", TLS: tc.mode}.withDefaults()
		if got.Port != tc.port {
			t.Errorf("%s/%s port = %d, want %d", tc.proto, tc.mode, got.Port, tc.port)
		}
		if got.Folder != DefaultFolder || got.AfterProcess != ActionKeep {
			t.Errorf("%s: folder=%q action=%q", tc.proto, got.Folder, got.AfterProcess)
		}
	}
}

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := NewFake([]byte("one"), []byte("two"), []byte("three"))

	msgs, err := f.Fetch(ctx, 2)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(msgs) != 2 || string(msgs[0].Raw) != "one" {
		t.Fatalf("Fetch: got %d messages, first %q", len(msgs), msgs[0].Raw)
	}

	// A kept message is not returned again, but stays in the mailbox.
	if err := f.Ack(ctx, []string{msgs[0].ID}, ActionKeep); err != nil {
		t.Fatalf("Ack keep: %v", err)
	}
	if err := f.Ack(ctx, []string{msgs[1].ID}, ActionDelete); err != nil {
		t.Fatalf("Ack delete: %v", err)
	}
	if f.Remaining() != 2 {
		t.Fatalf("Remaining = %d, want 2", f.Remaining())
	}
	msgs, err = f.Fetch(ctx, 0)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(msgs) != 1 || string(msgs[0].Raw) != "three" {
		t.Fatalf("second Fetch returned %d messages", len(msgs))
	}
	acks := f.Acks()
	if len(acks) != 2 || acks[1].Action != ActionDelete {
		t.Fatalf("Acks = %+v", acks)
	}

	f.FetchErr = errors.New("boom")
	if _, err := f.Fetch(ctx, 0); err == nil {
		t.Fatal("FetchErr ignored")
	}
}
