// Package llm talks to the gateway from the Railway side.
//
// The transport is deliberately boring: an ordinary HTTPS client with a bearer
// token. It reaches the Spark at a public hostname published by a Cloudflare
// Tunnel, so there is no private address to route to and nothing to set up on
// this side.
//
// It did not start that way. The gateway used to live on a tailnet, which meant
// a userspace tailscaled in the container (Railway has no TUN device), a SOCKS5
// proxy, ALL_PROXY plumbing, and an auth key with an expiry -- and when any of
// those lapsed, requests hung until they timed out, which is indistinguishable
// from the Spark being down. ProxyFromEnvironment below is retained so a
// same-tailnet deployment still works if anyone wants one, but it is no longer
// load-bearing.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
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

// Check validates the configuration before the first request, because most
// ways of getting this wrong present identically as "the model is down".
//
// It is advisory. The caller should log a failure and carry on: the simulation
// is the product and it runs fine with nobody generating opinions, so a
// gateway that is briefly unreachable must not stop the server from booting.
func (c *Client) Check(ctx context.Context) error {
	if c.BaseURL == "" {
		return errors.New("LLM_GATEWAY_URL is not set")
	}
	// A tailnet address with no proxy configured cannot work from a Railway
	// container: there is no TUN device, so 100.x has no kernel route and the
	// request hangs for the full timeout instead of failing. Name it now.
	if looksLikeTailnet(c.BaseURL) &&
		os.Getenv("ALL_PROXY") == "" && os.Getenv("all_proxy") == "" {
		return fmt.Errorf("%s looks like a tailnet address and no ALL_PROXY is "+
			"set -- without a TUN device there is no route and requests will "+
			"hang until they time out. Publish the gateway at a public hostname "+
			"(see deploy/setup-tunnel.sh)", c.BaseURL)
	}
	_, err := c.Health(ctx)
	return err
}

// looksLikeTailnet reports whether the URL's host is one that resolves only
// inside a tailnet: a 100.64.0.0/10 CGNAT address, or a bare MagicDNS name.
//
// Deliberately narrower than "is this a private address". Loopback and RFC1918
// are ordinary for a same-host or same-LAN deployment and must not warn, and
// they fail fast anyway if wrong. The tailnet case is singled out because it is
// the one that hangs for ten minutes rather than refusing the connection --
// silence there costs an afternoon, so it is worth naming up front.
func looksLikeTailnet(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); ip != nil {
		v4 := ip.To4()
		return v4 != nil && v4[0] == 100 && v4[1]&0xC0 == 64
	}
	// A name with no dots resolves only through MagicDNS or /etc/hosts.
	// "localhost" is the one everybody has, and it is not a tailnet.
	return h != "" && h != "localhost" && !strings.Contains(h, ".")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
