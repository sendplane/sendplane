package main

import (
	"bufio"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The campaign of scenario 4. It is small and goes through sender B
// (GreenMail) rather than through the 10,000-recipient chaos-smtp campaign of
// scenario 2 because the flows here need a mailbox the harness can read *and*
// a delivery it can then follow through the API: the pixel, the click and the
// one-click POST are all asserted against that one delivery's columns.
//
// The locale check lives in scenario 2, on the chaos campaign, where
// architecture 15 wanted it — cmd/chaos-smtp serves its recorded messages now
// (GAP-1 in README.md). These four recipients stay en/ko mixed anyway, so both
// render paths run before anything is clicked.
var trackingRecipients = []struct {
	email  string
	name   string
	locale string
}{
	{"trk-en-1@" + mailDomain, "Track EN One", "en"},
	{"trk-en-2@" + mailDomain, "Track EN Two", "en"},
	{"trk-ko-1@" + mailDomain, "추적 KO 일", "ko"},
	{"trk-ko-2@" + mailDomain, "추적 KO 이", "ko"},
}

func (r *runner) scenarioTracking(ctx context.Context) error {
	if err := r.runTrackingCampaign(ctx); err != nil {
		return err
	}
	return r.assertTrackingFlows(ctx)
}

func (r *runner) runTrackingCampaign(ctx context.Context) error {
	var camp campaign
	if err := r.api.postJSON(ctx, "/api/v1/campaigns", campaignInput{
		Name: "e2e-tracking", VersionID: r.versionID, SenderID: r.senderMailID,
	}, &camp); err != nil {
		return fmt.Errorf("create the tracking campaign: %w", err)
	}
	r.trackCampaignID = camp.ID

	var ing ingestResult
	err := r.api.postNDJSON(ctx, "/api/v1/campaigns/"+camp.ID+"/recipients", "tracking-0",
		func(w io.Writer) error {
			bw := bufio.NewWriter(w)
			for _, rc := range trackingRecipients {
				if _, err := fmt.Fprintf(bw, "{\"email\":%q,\"name\":%q,\"locale\":%q}\n",
					rc.email, rc.name, rc.locale); err != nil {
					return err
				}
			}
			return bw.Flush()
		}, &ing)
	if err != nil {
		return fmt.Errorf("ingest tracking recipients: %w", err)
	}
	if ing.Accepted != int64(len(trackingRecipients)) {
		return fmt.Errorf("ingest accepted %d of %d recipients", ing.Accepted, len(trackingRecipients))
	}

	if err := r.api.postJSON(ctx, "/api/v1/campaigns/"+camp.ID+"/start", map[string]any{}, nil); err != nil {
		return fmt.Errorf("start the tracking campaign: %w", err)
	}
	final, err := r.pollCampaign(ctx, camp.ID, 3*time.Minute)
	if err != nil {
		return err
	}
	if n := final.count("sent"); n != int64(len(trackingRecipients)) {
		r.fail("the tracking campaign sent %d of %d deliveries", n, len(trackingRecipients))
	}

	var list deliveryList
	if err := r.api.getJSON(ctx, "/api/v1/campaigns/"+camp.ID+"/deliveries?limit=50", &list); err != nil {
		return fmt.Errorf("list tracking deliveries: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Email < list.Items[j].Email })
	r.trackDeliveries = list.Items
	r.logf("  tracking campaign %s completed with %d deliveries", short(camp.ID), len(list.Items))
	return nil
}

// statsRefreshTimeout is how long the campaign aggregate may lag behind the
// interactions. The finalizer ticks every 10s by default and refreshes a
// campaign that completed within the last hour on every tick, so this is a
// generous multiple of one tick.
const statsRefreshTimeout = 90 * time.Second

var trackURLRe = regexp.MustCompile(`https://[a-z0-9.\-]+/t/[ocu]/[^"'<>\s)]+`)

// trackingURLs is what one rendered message carries (architecture 9.2).
type trackingURLs struct {
	pixel       string
	click       string
	unsubscribe string
	listUnsub   string
	oneClick    string
}

func extractTrackingURLs(msg gmMessage) (trackingURLs, error) {
	body, err := msg.htmlPart()
	if err != nil {
		return trackingURLs{}, err
	}
	var out trackingURLs
	for _, raw := range trackURLRe.FindAllString(body, -1) {
		u := html.UnescapeString(raw)
		switch {
		case strings.Contains(u, "/t/o/") && out.pixel == "":
			out.pixel = u
		case strings.Contains(u, "/t/c/") && out.click == "":
			out.click = u
		case strings.Contains(u, "/t/u/") && out.unsubscribe == "":
			out.unsubscribe = u
		}
	}
	out.listUnsub = msg.header("List-Unsubscribe")
	out.oneClick = msg.header("List-Unsubscribe-Post")
	if out.pixel == "" || out.click == "" || out.unsubscribe == "" {
		return out, fmt.Errorf("the rendered message is missing a tracking URL: %+v", out)
	}
	return out, nil
}

// toControl rewrites a tracking URL from the tenant's tracking domain onto the
// control listener. The public routes are registered on the path alone
// (internal/api/server.go mounts the generated chi router with no host
// matching), so a request to 127.0.0.1:18180 with the same path is the same
// request a real t.e2e.test would produce.
func (r *runner) toControl(u string) (string, error) {
	i := strings.Index(u, "/t/")
	if i < 0 {
		return "", fmt.Errorf("%q is not a tracking URL", u)
	}
	return u[i:], nil
}

func (r *runner) assertTrackingFlows(ctx context.Context) error {
	if len(r.trackDeliveries) < 2 {
		return fmt.Errorf("the tracking campaign produced %d deliveries, need at least 2", len(r.trackDeliveries))
	}
	target := r.trackDeliveries[0] // trk-en-1
	other := r.trackDeliveries[1]  // trk-en-2, used for the host-notify path

	msgs, err := r.gm.waitForMessage(ctx, target.Email, 1, r.opt.mailTimeout)
	if err != nil {
		return err
	}
	urls, err := extractTrackingURLs(msgs[0])
	if err != nil {
		return err
	}
	r.logf("  pixel=%s", trim(urls.pixel))
	r.logf("  click=%s", trim(urls.click))
	r.logf("  unsub=%s", trim(urls.unsubscribe))

	if !strings.HasPrefix(urls.pixel, "https://"+r.opt.trackingDomain+"/t/o/") {
		r.fail("the pixel URL %q is not on the tenant tracking domain", urls.pixel)
	}
	if !strings.Contains(urls.listUnsub, "/t/u/") {
		r.fail("List-Unsubscribe is %q, want the sendplane /t/u/ URL", urls.listUnsub)
	}
	if !strings.Contains(strings.ToLower(urls.oneClick), "one-click") {
		r.fail("List-Unsubscribe-Post is %q, want List-Unsubscribe=One-Click (RFC 8058)", urls.oneClick)
	}

	// internal/api's bot heuristic treats anything inside three seconds of
	// sent_at as a scanner (architecture 9.3), and such interactions are
	// excluded from the aggregate. Wait it out rather than pretend.
	r.waitOutBotWindow(target)

	hdr := map[string]string{"User-Agent": browserUA}

	// Open pixel: 200 image/gif, Cache-Control: no-store.
	pixelPath, err := r.toControl(urls.pixel)
	if err != nil {
		return err
	}
	resp, err := r.api.rawGet(ctx, pixelPath, hdr)
	if err != nil {
		return fmt.Errorf("GET pixel: %w", err)
	}
	if resp.Status != http.StatusOK {
		r.fail("GET pixel answered %d, want 200", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/gif" {
		r.fail("GET pixel Content-Type is %q, want image/gif", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		r.fail("GET pixel Cache-Control is %q, want no-store", cc)
	}
	if len(resp.Body) == 0 || string(resp.Body[:3]) != "GIF" {
		r.fail("GET pixel did not return a GIF (%d bytes)", len(resp.Body))
	}

	// Click: 302 to the signed destination.
	clickPath, err := r.toControl(urls.click)
	if err != nil {
		return err
	}
	resp, err = r.api.rawGet(ctx, clickPath, hdr)
	if err != nil {
		return fmt.Errorf("GET click: %w", err)
	}
	if resp.Status != http.StatusFound {
		r.fail("GET click answered %d, want 302", resp.Status)
	}
	if resp.Location != offerURL {
		r.fail("GET click redirected to %q, want %q", resp.Location, offerURL)
	}

	// Unsubscribe click: 302 to the host destination the token was signed with.
	unsubPath, err := r.toControl(urls.unsubscribe)
	if err != nil {
		return err
	}
	resp, err = r.api.rawGet(ctx, unsubPath, hdr)
	if err != nil {
		return fmt.Errorf("GET unsubscribe: %w", err)
	}
	if resp.Status != http.StatusFound {
		r.fail("GET unsubscribe answered %d, want 302", resp.Status)
	}
	wantDest := hostUnsubscribeBase + "?e=" + queryEscape(target.Email)
	if resp.Location != wantDest {
		r.fail("GET unsubscribe redirected to %q, want %q", resp.Location, wantDest)
	}

	// One-click POST: 200, and this is the one that confirms the unsubscribe.
	resp, err = r.api.rawPostForm(ctx, unsubPath, "List-Unsubscribe=One-Click", hdr)
	if err != nil {
		return fmt.Errorf("POST one-click unsubscribe: %w", err)
	}
	if resp.Status != http.StatusOK {
		r.fail("POST one-click unsubscribe answered %d, want 200", resp.Status)
	}

	// A tampered token must not be honoured.
	if tampered := tamper(clickPath); tampered != "" {
		resp, err = r.api.rawGet(ctx, tampered, hdr)
		if err != nil {
			return fmt.Errorf("GET tampered click: %w", err)
		}
		if resp.Status != http.StatusBadRequest {
			r.fail("a tampered click token answered %d, want 400", resp.Status)
		}
		if resp.Location != "" {
			r.fail("a tampered click token still redirected to %q", resp.Location)
		}
	}

	// The host-notify path of unsubscribe_mode=host (architecture 9.2), used
	// here on a second recipient so the confirmed count has to reach two.
	var notice unsubscribeResult
	if err := r.api.postJSON(ctx, "/api/v1/campaigns/"+r.trackCampaignID+"/unsubscribes",
		unsubscribeNotice{Email: other.Email, Source: "host"}, &notice); err != nil {
		return fmt.Errorf("POST campaign unsubscribes: %w", err)
	}
	if !notice.Recorded {
		r.fail("POST /campaigns/{id}/unsubscribes for %s reported recorded=false", other.Email)
	}

	r.assertTrackingRecorded(ctx, target, other)
	return nil
}

// assertTrackingRecorded checks the two places a recorded interaction shows
// up: the delivery's own single-writer columns, which the tracking handler
// updates synchronously, and the campaign aggregate.
func (r *runner) assertTrackingRecorded(ctx context.Context, target, other delivery) {
	// The tracking writer buffers for a second before inserting
	// (architecture 9.3), so give it a few.
	deadline := time.Now().Add(30 * time.Second)
	var d delivery
	for {
		if err := r.api.getJSON(ctx, "/api/v1/deliveries/"+target.ID, &d); err != nil {
			r.fail("re-reading delivery %s: %v", short(target.ID), err)
			return
		}
		if d.FirstOpenedAt != nil && d.FirstClickedAt != nil && d.UnsubscribedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if d.FirstOpenedAt == nil {
		r.fail("delivery %s has no first_opened_at after the pixel was fetched", short(target.ID))
	}
	if d.FirstClickedAt == nil {
		r.fail("delivery %s has no first_clicked_at after the click was followed", short(target.ID))
	}
	if d.UnsubscribedAt == nil {
		r.fail("delivery %s has no unsubscribed_at after the one-click POST", short(target.ID))
	}

	var od delivery
	if err := r.api.getJSON(ctx, "/api/v1/deliveries/"+other.ID, &od); err != nil {
		r.fail("re-reading delivery %s: %v", short(other.ID), err)
	} else if od.UnsubscribedAt == nil {
		r.fail("delivery %s has no unsubscribed_at after the host notification", short(other.ID))
	}

	// Click counts are computed live from the tracking table, not from the
	// campaign cache, so they are a real check of what was recorded.
	var links linkClickList
	if err := r.api.getJSON(ctx, "/api/v1/campaigns/"+r.trackCampaignID+"/links", &links); err != nil {
		r.fail("GET campaign links: %v", err)
	} else {
		found := false
		for _, l := range links.Items {
			if l.URL == offerURL {
				found = true
				if l.UniqueClicks != 1 {
					r.fail("link %q has unique_clicks=%d, want 1", l.URL, l.UniqueClicks)
				}
			}
		}
		if !found {
			r.fail("GET campaign links does not list %q", offerURL)
		}
	}

	// The campaign aggregate (architecture 9.3). Every interaction above
	// happened *after* the campaign completed, which is the normal case for
	// every real campaign: the finalizer keeps refreshing the tracking half of
	// a completed campaign's cached stats for a while, so these numbers have
	// to arrive. They are eventually consistent, not synchronous, so this
	// polls for a finalizer tick rather than reading once.
	want := campaignStats{UniqueOpens: 1, UniqueClicks: 1, Unsubscribed: 2, UnsubscribeClicked: 1}
	deadline = time.Now().Add(statsRefreshTimeout)
	var got campaignStats
	for {
		var c campaign
		if err := r.api.getJSON(ctx, "/api/v1/campaigns/"+r.trackCampaignID, &c); err != nil {
			r.fail("re-reading the tracking campaign: %v", err)
			return
		}
		got = campaignStats{}
		if c.Stats != nil {
			got = *c.Stats
		}
		if got.UniqueOpens == want.UniqueOpens && got.UniqueClicks == want.UniqueClicks &&
			got.Unsubscribed == want.Unsubscribed && got.UnsubscribeClicked == want.UnsubscribeClicked {
			r.logf("  campaign stats: opens=%d clicks=%d unsubscribed=%d unsubscribe_clicked=%d",
				got.UniqueOpens, got.UniqueClicks, got.Unsubscribed, got.UnsubscribeClicked)
			return
		}
		if time.Now().After(deadline) {
			r.fail("campaign stats are unique_opens=%d unique_clicks=%d unsubscribed=%d unsubscribe_clicked=%d "+
				"after %s, want 1/1/2/1: the finalizer is not refreshing the tracking uniques of a completed campaign",
				got.UniqueOpens, got.UniqueClicks, got.Unsubscribed, got.UnsubscribeClicked, statsRefreshTimeout)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// waitOutBotWindow sleeps until the delivery is older than internal/api's
// three-second bot window, so the open and click are counted.
func (r *runner) waitOutBotWindow(d delivery) {
	if d.SentAt == nil {
		time.Sleep(4 * time.Second)
		return
	}
	if wait := 4*time.Second - time.Since(*d.SentAt); wait > 0 {
		time.Sleep(wait)
	}
}

// tamper flips one character of the token so the HMAC cannot verify.
func tamper(path string) string {
	i := strings.Index(path, "/t/")
	if i < 0 {
		return ""
	}
	tok := path[i+5:]
	end := strings.IndexByte(tok, '?')
	query := ""
	if end >= 0 {
		query, tok = tok[end:], tok[:end]
	}
	if len(tok) < 6 {
		return ""
	}
	b := []byte(tok)
	mid := len(b) / 2
	if b[mid] == 'A' {
		b[mid] = 'B'
	} else {
		b[mid] = 'A'
	}
	return path[:i+5] + string(b) + query
}

func trim(s string) string {
	if len(s) > 96 {
		return s[:96] + "…"
	}
	return s
}

// pollCampaign polls until the campaign completes, printing progress.
func (r *runner) pollCampaign(ctx context.Context, id string, timeout time.Duration) (campaign, error) {
	deadline := time.Now().Add(timeout)
	var last campaign
	for {
		var c campaign
		if err := r.api.getJSON(ctx, "/api/v1/campaigns/"+id, &c); err != nil {
			r.logf("  poll failed (retrying): %v", err)
		} else {
			last = c
			if c.Status == "completed" && c.inFlight() == 0 {
				return c, nil
			}
			if c.Status == "cancelled" || c.Status == "paused" {
				return c, fmt.Errorf("campaign ended up %s", c.Status)
			}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("campaign %s is still %s after %s (sent=%d failed=%d in-flight=%d)",
				short(id), last.Status, timeout, last.count("sent"), last.count("failed"), last.inFlight())
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(r.opt.poll):
		}
	}
}
