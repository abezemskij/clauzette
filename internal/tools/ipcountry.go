package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const countryAPIBase = "https://api.country.is/"

// IPCountry looks up the country of an IP address with the api.country.is
// service. It is the first tool that reaches the network: the host is fixed and
// the only variable is an IP that must parse, so the request cannot be pointed
// at another host or path. Risk Exec, so it asks for operator approval before
// making the request (and, like exec_command, can be auto-approved or disabled
// by name in config).
type IPCountry struct {
	Client  *http.Client // defaults to a 10s client that refuses redirects
	BaseURL string       // defaults to countryAPIBase; overridden in tests
}

func (t *IPCountry) Name() string { return "ip_country" }
func (t *IPCountry) Risk() Risk   { return Exec }

func (t *IPCountry) Description() string {
	return "Look up the country of an IP address using the api.country.is service. Give an IPv4 or " +
		"IPv6 address, or omit it to look up the public IP this agent appears to come from. Returns the " +
		"IP and its ISO 3166-1 alpha-2 country code (for example US, GB, DE). Makes one HTTPS request to " +
		"api.country.is; requires operator approval."
}

func (t *IPCountry) Parameters() map[string]any {
	return objectSchema(nil, map[string]any{
		"ip": prop("string", "The IPv4 or IPv6 address to look up. Omit to look up this agent's own public IP."),
	})
}

func (t *IPCountry) Prepare(_ context.Context, args Args) (*Action, error) {
	ip, err := args.Str("ip", false)
	if err != nil {
		return nil, err
	}
	ip = strings.TrimSpace(ip)
	base := t.BaseURL
	if base == "" {
		base = countryAPIBase
	}
	url := base
	target := "this agent's own IP"
	if ip != "" {
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("%q is not a valid IP address", ip)
		}
		url += ip
		target = ip
	}
	return &Action{
		Summary: "look up the country of " + target + " via api.country.is",
		Preview: "GET " + url,
		Run: func(ctx context.Context) (string, error) {
			return t.lookup(ctx, url)
		},
	}, nil
}

func (t *IPCountry) lookup(ctx context.Context, url string) (string, error) {
	client := t.Client
	if client == nil {
		client = &http.Client{
			Timeout: 10 * time.Second,
			// Never follow a redirect: the whole point is that this talks only
			// to the one fixed host.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("refusing to follow a redirect away from api.country.is")
			},
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "clauzette")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to api.country.is failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("api.country.is did not recognise that IP (HTTP 404)")
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("api.country.is returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("could not parse the api.country.is response: %w", err)
	}
	if out.Country == "" {
		return "", fmt.Errorf("api.country.is returned no country for that IP")
	}
	return fmt.Sprintf("IP: %s\nCountry: %s (ISO 3166-1 alpha-2)", out.IP, out.Country), nil
}
