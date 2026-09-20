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
// internal/api: the load generator needs to stream a 50k-line NDJSON body and
// to see the raw status code of a replayed chunk, and both are easier with
// net/http directly than through the strict client's typed wrappers. The
// structs below therefore mirror api/openapi.yaml by hand; a field renamed in
// the spec shows up here as a zero value, which every assertion in main.go
// would catch.
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
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 64,
				MaxConnsPerHost:     64,
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

func (c *apiClient) getJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, "", nil, out)
}

func (c *apiClient) postJSON(ctx context.Context, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(raw), "application/json"
	}
	return c.do(ctx, http.MethodPost, path, body, ct, nil, out)
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
	defer resp.Body.Close()

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

// postNDJSON streams a recipient chunk. The body is produced by write on a
// separate goroutine through an io.Pipe so that neither the generator nor the
// server ever holds the whole chunk in memory (architecture 7.2).
func (c *apiClient) postNDJSON(
	ctx context.Context, path, idempotencyKey string,
	write func(w io.Writer) error, out any,
) error {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(write(pw))
	}()
	defer pr.Close()

	hdr := map[string]string{}
	if idempotencyKey != "" {
		hdr["Idempotency-Key"] = idempotencyKey
	}
	return c.do(ctx, http.MethodPost, path, pr, "application/x-ndjson", hdr, out)
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
}

type templateInput struct {
	Name          string `json:"name"`
	Subject       string `json:"subject"`
	Mode          string `json:"mode"`
	Body          string `json:"body"`
	Text          string `json:"text,omitempty"`
	DefaultLocale string `json:"default_locale,omitempty"`
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
	ByStatus   map[string]int64 `json:"by_status"`
	ComputedAt *time.Time       `json:"computed_at"`
}

type campaign struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	VersionID   string         `json:"version_id"`
	Stats       *campaignStats `json:"stats"`
	StartedAt   *time.Time     `json:"started_at"`
	CompletedAt *time.Time     `json:"completed_at"`
}

// count is by_status lookup that treats an absent status as zero: the
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
	Status         string     `json:"status"`
	Email          string     `json:"email"`
	AttemptCount   int32      `json:"attempt_count"`
	LastErrorClass string     `json:"last_error_class"`
	LastSMTPCode   int32      `json:"last_smtp_code"`
	SentAt         *time.Time `json:"sent_at"`
	CreatedAt      *time.Time `json:"created_at"`
}

type deliveryList struct {
	Items      []delivery `json:"items"`
	NextCursor string     `json:"next_cursor"`
}

type linkClick struct {
	LinkNo int32  `json:"link_no"`
	URL    string `json:"url"`
	Clicks int64  `json:"clicks"`
}

type linkClickList struct {
	Items []linkClick `json:"items"`
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("chaos-smtp /stats: HTTP %d", resp.StatusCode)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func queryEscape(s string) string { return url.QueryEscape(s) }
