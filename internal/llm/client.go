// Package llm talks to the gateway from the Railway side.
//
// The transport detail that matters: on Railway the app reaches the Spark over
// Tailscale running in userspace mode, which means there is no TUN device and
// no kernel route to 100.x addresses. Traffic has to go through tailscaled's
// local SOCKS5 proxy instead. Go's http.Transport understands a socks5:// URL
// from ALL_PROXY, so this needs no dependency and no custom dialer -- but it
// does need the environment variable to be set, and a silent absence looks
// exactly like the Spark being offline. Hence Client.Check.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

type Lane string

const (
	Interactive Lane = "interactive"
	Batch       Lane = "batch"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client

	// MaxRetries applies only to 503s from the gateway, which are admission
	// control rather than failure -- the correct response is to wait and come
	// back, not to give up or to hammer.
	MaxRetries int
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		HTTP: &http.Client{
			// ProxyFromEnvironment picks up ALL_PROXY=socks5://... which is
			// how userspace Tailscale is reached.
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: 16,
			},
			Timeout: 10 * time.Minute,
		},
		MaxRetries: 6,
	}
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Model       string            `json:"model"`
	Messages    []Message         `json:"messages"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature float64           `json:"temperature"`
	Extra       map[string]any    `json:"-"`
	Template    map[string]any    `json:"chat_template_kwargs,omitempty"`
	Stop        []string          `json:"stop,omitempty"`
	_           struct{}          `json:"-"`
	Headers     map[string]string `json:"-"`
}

type Response struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`

	// Cached reports whether the gateway served this without touching the GPU.
	// Worth surfacing: on a workload this repetitive the hit rate is the
	// difference between a five-minute batch and a five-hour one.
	Cached bool `json:"-"`
}

var ErrSaturated = errors.New("gateway saturated after retries")

// Complete sends one request. Batch callers should use Batch, which is the
// lane that respects the reservation.
func (c *Client) Complete(ctx context.Context, lane Lane, req Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	backoff := time.Second
	for attempt := 0; ; attempt++ {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Populace-Lane", string(lane))
		if c.Token != "" {
			r.Header.Set("Authorization", "Bearer "+c.Token)
		}

		resp, err := c.HTTP.Do(r)
		if err != nil {
			return nil, fmt.Errorf("gateway unreachable (check ALL_PROXY and that "+
				"tailscaled is up): %w", err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}

		switch resp.StatusCode {
		case http.StatusOK:
			var out Response
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fmt.Errorf("gateway returned unparseable JSON: %w", err)
			}
			out.Cached = resp.Header.Get("X-Populace-Cache") == "hit"
			return &out, nil

		case http.StatusServiceUnavailable:
			if attempt >= c.MaxRetries {
				return nil, ErrSaturated
			}
			wait := backoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if s, err := strconv.Atoi(ra); err == nil {
					wait = time.Duration(s) * time.Second
				}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}

		default:
			return nil, fmt.Errorf("gateway returned %d: %s",
				resp.StatusCode, truncate(string(raw), 300))
		}
	}
}

// Health is the gateway's view of itself. The relay should poll this and stop
// issuing batch work when the queue is deep, rather than discovering
// saturation by being shed.
func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// Check validates the environment before the first request, because the two
// most likely misconfigurations both present as "the model is down".
func (c *Client) Check(ctx context.Context) error {
	if c.BaseURL == "" {
		return errors.New("LLM_GATEWAY_URL is not set")
	}
	if os.Getenv("ALL_PROXY") == "" && os.Getenv("all_proxy") == "" {
		// Not fatal: a same-host deployment needs no proxy. But on Railway
		// this is the mistake, and it is worth naming rather than timing out.
		return fmt.Errorf("ALL_PROXY is unset -- if the gateway is on a tailnet, "+
			"userspace tailscaled has no kernel route and requests to %s will "+
			"hang until they time out", c.BaseURL)
	}
	_, err := c.Health(ctx)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
