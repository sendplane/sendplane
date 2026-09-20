package store_test

import (
	"errors"
	"testing"

	"github.com/sendplane/sendplane/store"
)

func TestNormalizeEmail(t *testing.T) {
	ok := []struct{ in, want string }{
		{"user@example.com", "user@example.com"},
		{"  User@Example.COM  ", "user@example.com"},
		{"Display Name <User@Example.com>", "user@example.com"},
		{"user@호스트.한국", "user@xn--9t4b270a0sc.xn--3e0b707e"},
		{"user+tag@sub.example.com", "user+tag@sub.example.com"},
	}
	for _, tc := range ok {
		got, err := store.NormalizeEmail(tc.in)
		if err != nil {
			t.Errorf("NormalizeEmail(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"",
		"   ",
		"not-an-email",
		"user@",
		"@example.com",
		"user@example.com\r\nBcc: victim@example.com",
		"user\n@example.com",
		"a b@example.com",
	}
	for _, in := range bad {
		if got, err := store.NormalizeEmail(in); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want ErrInvalid", in, got, err)
		}
	}
}
