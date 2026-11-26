// Package httpc fetches the meminfo JSON endpoint (Talos nodes run the
// meminfo-service system extension). Unlike the legacy "curl -ks" it has
// an enforced overall timeout, validates TLS by default, and supports an
// optional bearer token for protected deployments.
package httpc

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Options for one fetch.
type Options struct {
	URL      string
	Token    string
	Headers  map[string]string
	Insecure bool // skip TLS verification (explicit config decision, logged by caller)
	Timeout  time.Duration
}

// Fetch retrieves and JSON-validates the endpoint (parsing lives in
// memsrc so the wire protocol is defined once).
func Fetch(o Options) (string, error) {
	if o.URL == "" {
		return "", fmt.Errorf("http: url not configured")
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	client := &http.Client{
		Timeout: o.Timeout,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: o.Insecure},
			DialContext:         (&net.Dialer{Timeout: o.Timeout}).DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: o.Timeout,
		},
	}
	req, err := http.NewRequest(http.MethodGet, o.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pve-real-memory")
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	if o.Token != "" {
		req.Header.Set("Authorization", "Bearer "+o.Token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http fetch %s: %w", o.URL, err)
	}
	defer resp.Body.Close()

	// Hard cap: a broken endpoint must not balloon the daemon.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("http read %s: %w", o.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %s: status %d", o.URL, resp.StatusCode)
	}
	// Reject non-JSON early for better errors.
	var probe json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", fmt.Errorf("http %s: response is not JSON: %.120s", o.URL, body)
	}
	return string(body), nil
}
