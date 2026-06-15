package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/cli/go-gh/v2/pkg/api"
)

// requestFunc issues one HTTP request and returns the raw response. It matches
// go-gh's public RESTClient.RequestWithContext, the only mechanism this package
// uses to reach the GitHub API; depending on that public signature (rather than
// any internal transport wiring) keeps the retry loop entirely on the public
// surface and makes it injectable in tests.
type requestFunc func(ctx context.Context, method, path string, body io.Reader) (*http.Response, error)

// restClient is the go-gh-backed implementation of Client.
type restClient struct {
	request requestFunc
	policy  retryPolicy
}

// New builds a Client over the user's existing gh authentication and host
// configuration (GH_TOKEN / GH_HOST are honored by go-gh).
func New() (Client, error) {
	rc, err := api.DefaultRESTClient()
	if err != nil {
		return nil, fmt.Errorf("creating GitHub client: %w", err)
	}
	return &restClient{request: rc.RequestWithContext, policy: defaultPolicy()}, nil
}

// do issues a request with rate-limit-aware retry. A non-nil body is sent as
// JSON; a non-nil out receives the decoded successful response. The response
// headers are returned so callers can read values such as Link.
func (c *restClient) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding %s %s body: %w", method, path, err)
		}
		payload = b
	}

	var lastErr error
	for attempt := 1; attempt <= c.policy.maxAttempts; attempt++ {
		var r io.Reader
		if payload != nil {
			r = bytes.NewReader(payload)
		}
		resp, err := c.request(ctx, method, path, r)
		if err != nil {
			lastErr = err
			delay, retry := c.policy.retryDelay(method, err, attempt)
			if !retry || attempt == c.policy.maxAttempts {
				return nil, err
			}
			if werr := c.policy.wait(ctx, delay); werr != nil {
				return nil, werr
			}
			continue
		}
		return resp.Header, decode(resp, out, method, path)
	}
	return nil, lastErr
}

// decode reads (and always closes) a successful response body, unmarshaling into
// out when out is non-nil and draining it otherwise so the connection can be
// reused.
func decode(resp *http.Response, out any, method, path string) error {
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}

// notFound reports whether err is a 404 from the API.
func notFound(err error) bool {
	var he *api.HTTPError
	return errors.As(err, &he) && he.StatusCode == http.StatusNotFound
}
