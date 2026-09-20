package probe

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/sendplane/sendplane/store"
)

// Folder names, normalized from the mailbox's own folder mapping.
const (
	FolderInbox = "inbox"
	FolderSpam  = "spam"
	FolderOther = "other"
)

// Observation is what one probe mail told us, after the headers were parsed.
// It is the input to the verdict of architecture 11.4 and the source of the
// ProbeRun columns.
type Observation struct {
	Delivered bool
	// TrustedAR reports whether an Authentication-Results header with the
	// mailbox's authserv-id was found. Without one there is no verdict to
	// make, only a delivery.
	TrustedAR bool

	SPF   string
	DKIM  string
	DMARC string

	DKIMDomain   string
	DKIMSelector string
	DMARCPolicy  string
	MailFrom     string

	Folder     string
	TLS        bool
	ObservedIP net.IP
	PTR        string
	// PTRChecked is false when no DNS layer ran, which keeps a missing PTR
	// from being read as a mismatch.
	PTRChecked bool
	PTRMatch   bool

	Latency time.Duration
}

// Verdict applies architecture 11.4:
//
//	green   everything passed, landed in the inbox, arrived over TLS
//	yellow  dmarc p=none, spam folder, PTR mismatch, no TLS, nothing to judge
//	red     not delivered, or spf/dkim/dmarc reported fail
//
// The reason is the first thing that was wrong, worst first, because that is
// what an operator acts on. The rest stays on the ProbeRun.
func Verdict(o Observation) (store.HealthStatus, string) {
	if !o.Delivered {
		return store.HealthRed, "메일이 도착하지 않았습니다"
	}

	for _, c := range []struct{ name, result string }{
		{"dmarc", o.DMARC}, {"dkim", o.DKIM}, {"spf", o.SPF},
	} {
		switch normalizeAuthResult(c.result) {
		case "fail":
			return store.HealthRed, fmt.Sprintf("%s=fail", c.name)
		case "policy":
			return store.HealthRed, fmt.Sprintf("%s=policy", c.name)
		}
	}

	if !o.TrustedAR {
		return store.HealthYellow, "신뢰할 수 있는 Authentication-Results 헤더가 없습니다"
	}

	if o.Folder == FolderSpam {
		return store.HealthYellow, "스팸함으로 분류되었습니다"
	}
	for _, c := range []struct{ name, result string }{
		{"spf", o.SPF}, {"dkim", o.DKIM}, {"dmarc", o.DMARC},
	} {
		switch normalizeAuthResult(c.result) {
		case "softfail":
			return store.HealthYellow, fmt.Sprintf("%s=softfail", c.name)
		case "none", "":
			return store.HealthYellow, fmt.Sprintf("%s 판정이 없습니다", c.name)
		case "neutral", "permerror", "temperror":
			return store.HealthYellow, fmt.Sprintf("%s=%s", c.name, normalizeAuthResult(c.result))
		}
	}
	if o.DMARCPolicy == "none" {
		return store.HealthYellow, "DMARC 정책이 p=none입니다"
	}
	if !o.TLS {
		return store.HealthYellow, "TLS 없이 전달되었습니다"
	}
	if o.PTRChecked && !o.PTRMatch {
		return store.HealthYellow, "PTR가 정방향 조회와 일치하지 않습니다(FCrDNS)"
	}
	if o.Folder != FolderInbox {
		return store.HealthYellow, "받은편지함이 아닌 폴더로 분류되었습니다"
	}
	return store.HealthGreen, "spf/dkim/dmarc pass, 받은편지함, TLS"
}

func normalizeAuthResult(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// worst is the summary of several mailboxes' verdicts (architecture 11.2:
// "여러 메일박스 결과의 최악 값이 요약 상태"). HealthStatus is ordered
// unknown < green < yellow < red, so worst is a max.
func worst(a, b store.HealthStatus) store.HealthStatus {
	if b > a {
		return b
	}
	return a
}

// folderKind maps a mailbox's own folder name onto inbox/spam/other.
func folderKind(m *store.ProbeMailbox, folder string) string {
	switch {
	case folder == "":
		return ""
	case m != nil && m.SpamFolder != "" && strings.EqualFold(folder, m.SpamFolder):
		return FolderSpam
	case m != nil && m.InboxFolder != "" && strings.EqualFold(folder, m.InboxFolder):
		return FolderInbox
	}
	switch strings.ToLower(folder) {
	case FolderInbox:
		return FolderInbox
	case FolderSpam, "junk", "[gmail]/spam", "junk email", "junk e-mail":
		return FolderSpam
	}
	return FolderOther
}
