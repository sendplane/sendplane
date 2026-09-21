package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiClient is a hand-written client rather than the generated one from
// internal/api, for the same reason test/load/client.go is: the harness needs
// to stream NDJSON, to see raw status codes (a tampered tracking token has to
// answer 400, not an error value) and to stop a redirect from being followed.
// The structs below mirror api/openapi.yaml by hand; a field renamed in the
// spec shows up here as a zero value, which the assertions catch.
type apiClient struct {
	base string
	hc   *http.Client
}

func newAPIClient(base string, timeout time.Duration) *apiClient {
	return &apiClient{
		base: strings.TrimRight(base, "/"),
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 32,
			},
		},
	}
}

// apiError carries the status code so a caller can tell "not ready yet" from
// "wrong request".
type apiError struct {
	Status int
	Code   string
	Msg    string
	Body   string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

func statusOf(err error) int {
	var ae *apiError
	if ok := asAPIError(err, &ae); ok {
		return ae.Status
	}
	return 0
}

func asAPIError(err error, out **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok { //nolint:errorlint // the client never wraps
			*out = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (c *apiClient) getJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, "", nil, out)
}

func (c *apiClient) postJSON(ctx context.Context, path string, in, out any) error {
	return c.postJSONWith(ctx, path, in, nil, out)
}

func (c *apiClient) postJSONWith(ctx context.Context, path string, in any, hdr map[string]string, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(raw), "application/json"
	}
	return c.do(ctx, http.MethodPost, path, body, ct, hdr, out)
}

func (c *apiClient) putJSON(ctx context.Context, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, path, bytes.NewReader(raw), "application/json", nil, out)
}

func (c *apiClient) do(
	ctx context.Context, method, path string, body io.Reader, contentType string,
	header map[string]string, out any,
) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		e := &apiError{Status: resp.StatusCode, Body: string(raw)}
		var perr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &perr) == nil {
			e.Code, e.Msg = perr.Code, perr.Message
		}
		return e
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// postNDJSON streams a recipient chunk through an io.Pipe so neither side ever
// holds the whole chunk in memory (architecture 7.2).
func (c *apiClient) postNDJSON(
	ctx context.Context, path, idempotencyKey string,
	write func(w io.Writer) error, out any,
) error {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(write(pw))
	}()
	defer func() { _ = pr.Close() }()

	hdr := map[string]string{}
	if idempotencyKey != "" {
		hdr["Idempotency-Key"] = idempotencyKey
	}
	return c.do(ctx, http.MethodPost, path, pr, "application/x-ndjson", hdr, out)
}

// rawResponse is what the tracking assertions need: a status code, the
// Location header of a redirect that was deliberately not followed, and the
// body bytes.
type rawResponse struct {
	Status   int
	Location string
	Header   http.Header
	Body     []byte
}

// rawGet performs a GET without following redirects. The tracking routes
// answer 302 and the destination is the assertion, so following it would both
// lose the Location and send the harness off to a host that does not exist.
func (c *apiClient) rawGet(ctx context.Context, path string, header map[string]string) (rawResponse, error) {
	return c.raw(ctx, http.MethodGet, path, nil, "", header)
}

func (c *apiClient) rawPostForm(ctx context.Context, path, form string, header map[string]string) (rawResponse, error) {
	return c.raw(ctx, http.MethodPost, path, strings.NewReader(form),
		"application/x-www-form-urlencoded", header)
}

func (c *apiClient) raw(
	ctx context.Context, method, path string, body io.Reader, contentType string, header map[string]string,
) (rawResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return rawResponse{}, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	// One client per call: CheckRedirect is a client-wide setting and the
	// JSON calls do want redirects followed.
	hc := &http.Client{
		Timeout:       c.hc.Timeout,
		Transport:     c.hc.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := hc.Do(req)
	if err != nil {
		return rawResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return rawResponse{}, err
	}
	return rawResponse{
		Status: resp.StatusCode, Location: resp.Header.Get("Location"),
		Header: resp.Header, Body: raw,
	}, nil
}

// --- API shapes (api/openapi.yaml) --------------------------------------

type retryPolicy struct {
	Backoff     []string `json:"backoff,omitempty"`
	MaxAttempts int32    `json:"max_attempts,omitempty"`
}

type signingKeyInfo struct {
	Kid    string `json:"kid"`
	Secret string `json:"secret,omitempty"`
}

type trackingConfig struct {
	Domain      string           `json:"domain,omitempty"`
	Opens       *bool            `json:"opens,omitempty"`
	Clicks      *bool            `json:"clicks,omitempty"`
	SigningKeys []signingKeyInfo `json:"signing_keys,omitempty"`
}

type tenantSettings struct {
	TenantID               string          `json:"tenant_id,omitempty"`
	Retry                  *retryPolicy    `json:"retry,omitempty"`
	RetentionDays          int32           `json:"retention_days,omitempty"`
	SuppressionEnabled     *bool           `json:"suppression_enabled,omitempty"`
	UnsubscribeMode        string          `json:"unsubscribe_mode,omitempty"`
	UnsubscribeURLTemplate string          `json:"unsubscribe_url_template,omitempty"`
	UnsubscribeOneClick    *bool           `json:"unsubscribe_one_click,omitempty"`
	DefaultLocale          string          `json:"default_locale,omitempty"`
	Tracking               *trackingConfig `json:"tracking,omitempty"`
	Version                int64           `json:"version"`
}

type transportInput struct {
	Name          string  `json:"name"`
	Host          string  `json:"host"`
	Port          int32   `json:"port"`
	TLS           string  `json:"tls,omitempty"`
	MaxConns      int32   `json:"max_conns,omitempty"`
	RatePerSecond float64 `json:"rate_per_second,omitempty"`
}

type senderInput struct {
	Name        string `json:"name"`
	FromName    string `json:"from_name,omitempty"`
	FromEmail   string `json:"from_email"`
	TransportID string `json:"transport_id"`
	DomainID    string `json:"domain_id,omitempty"`
}

type sendingDomainInput struct {
	Domain           string `json:"domain"`
	ReturnPathDomain string `json:"return_path_domain,omitempty"`
}

type i18nBundle struct {
	DefaultLocale string                       `json:"default_locale,omitempty"`
	Locales       map[string]map[string]string `json:"locales,omitempty"`
}

type templateInput struct {
	Name          string      `json:"name"`
	Subject       string      `json:"subject"`
	Mode          string      `json:"mode"`
	Body          string      `json:"body"`
	Text          string      `json:"text,omitempty"`
	I18n          *i18nBundle `json:"i18n,omitempty"`
	DefaultLocale string      `json:"default_locale,omitempty"`
}

type idOnly struct {
	ID string `json:"id"`
}

type messageVersion struct {
	ID    string   `json:"id"`
	Links []string `json:"links"`
}

type campaignInput struct {
	Name      string `json:"name"`
	VersionID string `json:"version_id,omitempty"`
	SenderID  string `json:"sender_id"`
}

type campaignStats struct {
	ByStatus           map[string]int64 `json:"by_status"`
	UniqueOpens        int64            `json:"unique_opens"`
	UniqueClicks       int64            `json:"unique_clicks"`
	Unsubscribed       int64            `json:"unsubscribed"`
	UnsubscribeClicked int64            `json:"unsubscribe_clicked"`
	ComputedAt         *time.Time       `json:"computed_at"`
}

type campaign struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	VersionID   string         `json:"version_id"`
	Stats       *campaignStats `json:"stats"`
	StartedAt   *time.Time     `json:"started_at"`
	CompletedAt *time.Time     `json:"completed_at"`
}

// count is a by_status lookup that treats an absent status as zero: the
// aggregate only carries the statuses it actually saw.
func (c *campaign) count(status string) int64 {
	if c.Stats == nil {
		return 0
	}
	return c.Stats.ByStatus[status]
}

func (c *campaign) inFlight() int64 {
	var n int64
	for _, s := range []string{"pending", "queued", "leased", "deferred"} {
		n += c.count(s)
	}
	return n
}

type ingestResult struct {
	Accepted         int64 `json:"accepted"`
	Duplicates       int64 `json:"duplicates"`
	Invalid          int64 `json:"invalid"`
	Total            int64 `json:"total"`
	IdempotentReplay bool  `json:"idempotent_replay"`
}

type delivery struct {
	ID             string     `json:"id"`
	CampaignID     string     `json:"campaign_id"`
	Status         string     `json:"status"`
	Email          string     `json:"email"`
	Locale         string     `json:"locale"`
	MessageID      string     `json:"message_id"`
	AttemptCount   int32      `json:"attempt_count"`
	LastErrorClass string     `json:"last_error_class"`
	LastError      string     `json:"last_error"`
	SentAt         *time.Time `json:"sent_at"`
	// The conditional single-writer columns of architecture 9.3. Unlike the
	// campaign stats cache these are written by the tracking handler itself,
	// so they are current the moment the request returns.
	FirstOpenedAt  *time.Time `json:"first_opened_at"`
	FirstClickedAt *time.Time `json:"first_clicked_at"`
	UnsubscribedAt *time.Time `json:"unsubscribed_at"`
	CreatedAt      *time.Time `json:"created_at"`
}

type linkClick struct {
	LinkNo       int32  `json:"link_no"`
	URL          string `json:"url"`
	Clicks       int64  `json:"clicks"`
	UniqueClicks int64  `json:"unique_clicks"`
}

type linkClickList struct {
	Items []linkClick `json:"items"`
}

type deliveryList struct {
	Items      []delivery `json:"items"`
	NextCursor string     `json:"next_cursor"`
}

type messageRecipient struct {
	Email  string         `json:"email"`
	Name   string         `json:"name,omitempty"`
	Locale string         `json:"locale,omitempty"`
	Vars   map[string]any `json:"vars,omitempty"`
}

type messageRequest struct {
	TemplateID string             `json:"template_id,omitempty"`
	VersionID  string             `json:"version_id,omitempty"`
	SenderID   string             `json:"sender_id"`
	To         []messageRecipient `json:"to"`
	Headers    map[string]string  `json:"headers,omitempty"`
}

type messageResultItem struct {
	DeliveryID string `json:"delivery_id"`
	Email      string `json:"email"`
	Status     string `json:"status"`
}

type messageResult struct {
	VersionID        string              `json:"version_id"`
	Deliveries       []messageResultItem `json:"deliveries"`
	IdempotentReplay bool                `json:"idempotent_replay"`
}

type bounceEvent struct {
	ID             string `json:"id"`
	DeliveryID     string `json:"delivery_id"`
	Type           string `json:"type"`
	Source         string `json:"source"`
	Verified       bool   `json:"verified"`
	Recipient      string `json:"recipient"`
	EmailNorm      string `json:"email_norm"`
	SMTPStatus     string `json:"smtp_status"`
	DiagnosticCode string `json:"diagnostic_code"`
}

type bounceEventList struct {
	Items []bounceEvent `json:"items"`
}

type suppression struct {
	EmailNorm        string `json:"email_norm"`
	Reason           string `json:"reason"`
	SourceDeliveryID string `json:"source_delivery_id"`
}

type unsubscribeNotice struct {
	DeliveryID string `json:"delivery_id,omitempty"`
	Email      string `json:"email,omitempty"`
	Source     string `json:"source,omitempty"`
}

type unsubscribeResult struct {
	DeliveryID string `json:"delivery_id"`
	Recorded   bool   `json:"recorded"`
	Suppressed bool   `json:"suppressed"`
}

type probeRun struct {
	ID         string     `json:"id"`
	SenderID   string     `json:"sender_id"`
	MailboxID  string     `json:"mailbox_id"`
	DeliveryID string     `json:"delivery_id"`
	GroupID    string     `json:"group_id"`
	Pending    bool       `json:"pending"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason"`
	Delivered  bool       `json:"delivered"`
	Folder     string     `json:"folder"`
	Latency    string     `json:"latency"`
	SPF        string     `json:"spf"`
	DKIM       string     `json:"dkim"`
	DMARC      string     `json:"dmarc"`
	ReceivedAt *time.Time `json:"received_at"`
}

type probeTriggerResult struct {
	Runs []struct {
		RunID      string `json:"run_id"`
		MailboxID  string `json:"mailbox_id"`
		DeliveryID string `json:"delivery_id"`
	} `json:"runs"`
}

type senderHealth struct {
	SenderID  string     `json:"sender_id"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason"`
	CheckedAt *time.Time `json:"checked_at"`
	Mailboxes []probeRun `json:"mailboxes"`
}

type outboxEvent struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	Attempts  int32           `json:"attempts"`
	LastError string          `json:"last_error"`
	Payload   json.RawMessage `json:"payload"`
}

type outboxEventList struct {
	Items []outboxEvent `json:"items"`
}

// chaosStats mirrors chaossmtp.Stats as cmd/chaos-smtp serves it on /stats.
type chaosStats struct {
	Connections int64 `json:"Connections"`
	Messages    int64 `json:"Messages"`
	Accepted    int64 `json:"Accepted"`
	TempFailed  int64 `json:"TempFailed"`
	PermFailed  int64 `json:"PermFailed"`
	Dropped     int64 `json:"Dropped"`
	RateLimited int64 `json:"RateLimited"`
	AuthFailed  int64 `json:"AuthFailed"`
}

func fetchChaosStats(ctx context.Context, hc *http.Client, base string) (chaosStats, error) {
	var out chaosStats
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/stats", nil)
	if err != nil {
		return out, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("chaos-smtp /stats: HTTP %d", resp.StatusCode)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func queryEscape(s string) string { return url.QueryEscape(s) }

// chaosMessage is one entry of cmd/chaos-smtp's GET /messages: what the relay
// actually received, which is the only way to assert on a rendered message
// that went to chaos-smtp rather than to GreenMail.
type chaosMessage struct {
	From      string            `json:"from"`
	Rcpts     []string          `json:"rcpts"`
	MessageID string            `json:"message_id"`
	Subject   string            `json:"subject"`
	Size      int               `json:"size"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
}

type chaosMessageList struct {
	Total int            `json:"total"`
	Items []chaosMessage `json:"items"`
}

// fetchChaosMessages reads the messages chaos-smtp remembered for one
// recipient. withBody needs the server to have been started with
// --keep-bodies (see test/e2e/docker-compose.yml).
func fetchChaosMessages(ctx context.Context, hc *http.Client, base, rcpt string, limit int, withBody bool) ([]chaosMessage, error) {
	u := fmt.Sprintf("%s/messages?rcpt=%s&limit=%d", strings.TrimRight(base, "/"), url.QueryEscape(rcpt), limit)
	if withBody {
		u += "&body=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chaos-smtp /messages: HTTP %d", resp.StatusCode)
	}
	var out chaosMessageList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
