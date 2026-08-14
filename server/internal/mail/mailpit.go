package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// pollInterval is how often WaitForLatestTo re-polls the inbox. Mail delivery
// through even a local Mailpit is asynchronous relative to Send returning, so
// a caller that just sent a message must poll rather than assume it has
// already landed.
const pollInterval = 200 * time.Millisecond

// Client is a small REST client for Mailpit's inbox. It exists for tests: the
// OTP loop and the signup-verify flow read codes/links back out of Mailpit
// programmatically instead of a human checking a web UI.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient builds a client against Mailpit's HTTP API root, e.g.
// "http://localhost:8026" (DRIVE_MAILPIT_API).
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Address is a name/address pair the way Mailpit reports it.
type Address struct {
	Name    string `json:"Name"`
	Address string `json:"Address"`
}

// Summary is one row of the inbox listing (GET /api/v1/messages).
type Summary struct {
	ID      string    `json:"ID"`
	From    Address   `json:"From"`
	To      []Address `json:"To"`
	Subject string    `json:"Subject"`
	Created time.Time `json:"Created"`
}

// Message is a full message, fetched by id (GET /api/v1/message/{id}).
type Message struct {
	Summary
	Text string `json:"Text"`
	HTML string `json:"HTML"`
}

type listResponse struct {
	Messages []Summary `json:"messages"`
}

// List returns the inbox, newest first (Mailpit's default sort).
func (c *Client) List(ctx context.Context) ([]Summary, error) {
	var out listResponse
	if err := c.getJSON(ctx, "/api/v1/messages", &out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// Get fetches one message's full body by id.
func (c *Client) Get(ctx context.Context, id string) (*Message, error) {
	var out Message
	if err := c.getJSON(ctx, "/api/v1/message/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAll purges every message in the inbox. Global test setup calls this so
// a prior failed run -- a kill-9 test included -- never leaks mail into the
// next one, and so the OTP loop always reads the code it just triggered.
func (c *Client) DeleteAll(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/v1/messages",
		bytes.NewReader([]byte(`{"IDs":[]}`)))
	if err != nil {
		return fmt.Errorf("mailpit: build delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("mailpit: delete all: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mailpit: delete all: status %d", resp.StatusCode)
	}
	return nil
}

// LatestTo returns the newest message addressed to addr. It errors if none
// exists yet -- callers that just sent mail want WaitForLatestTo instead.
func (c *Client) LatestTo(ctx context.Context, addr string) (*Message, error) {
	list, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	best := newestTo(list, addr, time.Time{})
	if best == nil {
		return nil, fmt.Errorf("mailpit: no message to %s", addr)
	}
	return c.Get(ctx, best.ID)
}

// WaitForLatestTo polls until a message to addr created at or after since
// appears, or ctx is done first. since should be a timestamp taken just before
// triggering the send, so a stale message from an earlier test run can never
// be mistaken for the one under test.
func (c *Client) WaitForLatestTo(ctx context.Context, addr string, since time.Time) (*Message, error) {
	for {
		if list, err := c.List(ctx); err == nil {
			if best := newestTo(list, addr, since); best != nil {
				return c.Get(ctx, best.ID)
			}
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("mailpit: no message to %s within deadline: %w", addr, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// newestTo returns the newest summary addressed to addr with Created >= since,
// or nil.
func newestTo(list []Summary, addr string, since time.Time) *Summary {
	var best *Summary
	for i := range list {
		s := &list[i]
		if s.Created.Before(since) || !addressedTo(s, addr) {
			continue
		}
		if best == nil || s.Created.After(best.Created) {
			best = s
		}
	}
	return best
}

func addressedTo(s *Summary, addr string) bool {
	for _, a := range s.To {
		if strings.EqualFold(a.Address, addr) {
			return true
		}
	}
	return false
}

func (c *Client) getJSON(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("mailpit: build request for %s: %w", path, err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("mailpit: get %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mailpit: get %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
