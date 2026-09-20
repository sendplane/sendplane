package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/sendplane/sendplane/store"
)

// DMARC checks _dmarc.{domain}: the record exists, declares a policy, asks for
// reports, and is not still parked at p=none (architecture 11.4 counts that as
// yellow, because a p=none domain gets no protection from a DMARC pass).
func (c *Checker) DMARC(ctx context.Context, domain string) Result {
	name := "_dmarc." + domain
	details := map[string]string{"domain": domain, "name": name}

	txts, err := c.r.TXT(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNoRecord) {
			return result("dmarc", store.HealthRed,
				fmt.Sprintf("%s에 DMARC 레코드가 없습니다", name), details)
		}
		details["error"] = err.Error()
		return result("dmarc", store.HealthYellow, "DMARC 레코드를 조회하지 못했습니다", details)
	}

	var records []string
	for _, t := range txts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), "v=dmarc1") {
			records = append(records, t)
		}
	}
	switch len(records) {
	case 0:
		return result("dmarc", store.HealthRed,
			fmt.Sprintf("%s의 TXT가 DMARC 레코드가 아닙니다", name), details)
	case 1:
	default:
		details["count"] = fmt.Sprint(len(records))
		return result("dmarc", store.HealthRed,
			"DMARC 레코드가 여러 개라 수신 측이 전부 무시합니다", details)
	}
	record := records[0]
	details["record"] = record

	tags := parseTagValue(record)
	policy := strings.ToLower(tags["p"])
	details["p"] = policy
	// adkim and aspf default to relaxed (RFC 7489 6.3).
	adkim, aspf := strings.ToLower(tags["adkim"]), strings.ToLower(tags["aspf"])
	if adkim == "" {
		adkim = "r"
	}
	if aspf == "" {
		aspf = "r"
	}
	details["adkim"], details["aspf"] = adkim, aspf
	if v := tags["sp"]; v != "" {
		details["sp"] = strings.ToLower(v)
	}
	if v := tags["pct"]; v != "" {
		details["pct"] = v
	}
	rua := tags["rua"]
	details["rua"] = rua

	switch policy {
	case "":
		return result("dmarc", store.HealthRed, "DMARC 레코드에 p= 태그가 없습니다", details)
	case "none":
		return result("dmarc", store.HealthYellow,
			"DMARC 정책이 p=none입니다(모니터링 전용)", details)
	case "quarantine", "reject":
	default:
		return result("dmarc", store.HealthRed,
			fmt.Sprintf("알 수 없는 DMARC 정책 p=%s", policy), details)
	}
	if rua == "" {
		return result("dmarc", store.HealthYellow,
			fmt.Sprintf("DMARC p=%s이지만 rua= 집계 리포트 주소가 없습니다", policy), details)
	}
	return result("dmarc", store.HealthGreen,
		fmt.Sprintf("DMARC p=%s, adkim=%s, aspf=%s", policy, adkim, aspf), details)
}

// MX checks that the return-path domain can receive DSNs at all: without an MX
// the bounce mailbox never sees a bounce, and bounce processing silently does
// nothing (architecture 11.3).
func (c *Checker) MX(ctx context.Context, domain string) Result {
	details := map[string]string{"domain": domain}

	mxs, err := c.r.MX(ctx, domain)
	if err != nil {
		if errors.Is(err, ErrNoRecord) {
			return result("mx", store.HealthRed,
				fmt.Sprintf("%s에 MX 레코드가 없어 바운스를 받을 수 없습니다", domain), details)
		}
		details["error"] = err.Error()
		return result("mx", store.HealthYellow, "MX 레코드를 조회하지 못했습니다", details)
	}

	hosts := make([]string, 0, len(mxs))
	for _, mx := range mxs {
		hosts = append(hosts, fmt.Sprintf("%d %s", mx.Pref, mx.Host))
	}
	details["hosts"] = strings.Join(hosts, ", ")

	// RFC 7505: a single "." target is an explicit "this domain accepts no
	// mail", which for a return-path domain means every DSN is refused.
	if len(mxs) == 1 && (mxs[0].Host == "" || mxs[0].Host == ".") {
		return result("mx", store.HealthRed,
			fmt.Sprintf("%s가 null MX(RFC 7505)라 바운스를 받지 않습니다", domain), details)
	}
	return result("mx", store.HealthGreen,
		fmt.Sprintf("%s의 MX %d개", domain, len(mxs)), details)
}

// PTR does the reverse lookup of the observed outbound IP and forward-confirms
// it (FCrDNS). A mismatch is yellow, not red: plenty of mail is accepted
// without it, but every large receiver scores it, and it is the usual
// explanation for a probe that lands in spam with all three of SPF, DKIM and
// DMARC passing.
func (c *Checker) PTR(ctx context.Context, ip net.IP) Result {
	details := map[string]string{"ip": ip.String()}

	name, err := ReverseName(ip)
	if err != nil {
		details["error"] = err.Error()
		return result("ptr", store.HealthYellow, "역방향 조회 이름을 만들 수 없습니다", details)
	}
	details["name"] = name

	names, err := c.r.PTR(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNoRecord) {
			return result("ptr", store.HealthYellow,
				fmt.Sprintf("%s에 PTR 레코드가 없습니다", ip), details)
		}
		details["error"] = err.Error()
		return result("ptr", store.HealthYellow, "PTR 레코드를 조회하지 못했습니다", details)
	}
	details["ptr"] = strings.Join(names, ", ")

	for _, host := range names {
		var addrs []net.IP
		var ferr error
		if ip.To4() != nil {
			addrs, ferr = c.r.A(ctx, host)
		} else {
			addrs, ferr = c.r.AAAA(ctx, host)
		}
		if ferr != nil {
			continue
		}
		for _, got := range addrs {
			if got.Equal(ip) {
				details["fcrdns"] = "true"
				details["match"] = host
				return result("ptr", store.HealthGreen,
					fmt.Sprintf("%s → %s → %s (FCrDNS 일치)", ip, host, ip), details)
			}
		}
	}
	details["fcrdns"] = "false"
	return result("ptr", store.HealthYellow,
		fmt.Sprintf("%s의 PTR(%s)이 정방향에서 같은 IP로 돌아오지 않습니다(FCrDNS 불일치)",
			ip, strings.Join(names, ", ")), details)
}
