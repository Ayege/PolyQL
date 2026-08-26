package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultGrafanaTimeout bounds a fetch. A dashboard is a single small document,
// so a request still running after this is a misrouted URL or an unresponsive
// instance rather than a slow one, and hanging the CLI on it helps nobody.
const DefaultGrafanaTimeout = 30 * time.Second

// GrafanaClient reads dashboards from a Grafana instance over its HTTP API.
//
// It is deliberately read-only. Writing a dashboard back is a destructive
// operation in a way translating a file is not — a bad translation would
// overwrite the panel expressions people are on call with — so pushing belongs
// behind its own explicit command rather than falling out of a fetch.
type GrafanaClient struct {
	// BaseURL is the Grafana root, with or without a trailing slash.
	BaseURL string
	// Token is a Grafana service-account token or API key. It is sent as a
	// bearer token, and is read from the environment rather than a flag by the
	// callers in this repository: an argument is visible in shell history and to
	// anything that can list processes.
	Token string
	// HTTP is the client used for the request. A nil value gets one with
	// DefaultGrafanaTimeout.
	HTTP *http.Client
}

// grafanaDashboardResponse is the envelope GET /api/dashboards/uid/:uid returns.
// The dashboard itself is nested under a key, alongside metadata this does not
// need.
type grafanaDashboardResponse struct {
	Dashboard json.RawMessage `json:"dashboard"`
	Meta      struct {
		Slug string `json:"slug"`
		URL  string `json:"url"`
	} `json:"meta"`
}

// maxDashboardBytes caps what will be read from a response. A Grafana dashboard
// is JSON measured in kilobytes; anything of this size is a wrong URL returning
// something else, and reading it all into memory first would be the failure.
const maxDashboardBytes = 32 << 20 // 32 MiB

// Dashboard fetches one dashboard by its UID.
func (c *GrafanaClient) Dashboard(ctx context.Context, uid string) (*Dashboard, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		return nil, fmt.Errorf("dashboard: a Grafana base URL is required")
	}
	if strings.TrimSpace(uid) == "" {
		return nil, fmt.Errorf("dashboard: a dashboard UID is required")
	}

	endpoint, err := c.dashboardURL(uid)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("dashboard: building the request for %s: %w", endpoint, err)
	}
	request.Header.Set("Accept", "application/json")
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultGrafanaTimeout}
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("dashboard: fetching %s: %w", endpoint, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, c.statusError(response, endpoint, uid)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxDashboardBytes))
	if err != nil {
		return nil, fmt.Errorf("dashboard: reading %s: %w", endpoint, err)
	}

	var envelope grafanaDashboardResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("dashboard: %s did not return a Grafana dashboard response: %w",
			endpoint, err)
	}
	if len(envelope.Dashboard) == 0 {
		// A 200 carrying no dashboard key means the URL reached something that
		// is not this API — a login page, a proxy, the wrong host.
		return nil, fmt.Errorf("dashboard: %s returned no \"dashboard\" field; "+
			"check that the URL is a Grafana root and not a dashboard page", endpoint)
	}

	return ParseDashboard(envelope.Dashboard, endpoint)
}

// dashboardURL builds the API endpoint for a UID.
func (c *GrafanaClient) dashboardURL(uid string) (string, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"))
	if err != nil {
		return "", fmt.Errorf("dashboard: %q is not a valid Grafana URL: %w", c.BaseURL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("dashboard: %q is not a valid Grafana URL: "+
			"give a full address such as https://grafana.example.com", c.BaseURL)
	}
	// Path and RawPath are set together: Path holds the decoded form and RawPath
	// the escaped one, which is what URL.String writes. Setting only Path would
	// leave a UID containing a slash to change the path structure, and setting
	// only Path to an already-escaped string would escape the percent signs
	// again — "a/b" becoming "a%252Fb".
	// Both prefixes are read before either field is written, since EscapedPath
	// derives from Path and would otherwise reflect the half-built value.
	const apiPath = "/api/dashboards/uid/"
	decodedPrefix := strings.TrimRight(base.Path, "/")
	escapedPrefix := strings.TrimRight(base.EscapedPath(), "/")

	base.Path = decodedPrefix + apiPath + uid
	base.RawPath = escapedPrefix + apiPath + url.PathEscape(uid)
	return base.String(), nil
}

// statusError turns a non-200 into a message naming the likely cause, since the
// three that actually happen are distinguishable and each has a different fix.
func (c *GrafanaClient) statusError(response *http.Response, endpoint, uid string) error {
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		if c.Token == "" {
			return fmt.Errorf("dashboard: %s refused the request (%s) and no token was sent; "+
				"set GRAFANA_TOKEN to a service-account token", endpoint, response.Status)
		}
		return fmt.Errorf("dashboard: %s refused the token (%s); "+
			"check that it is current and may read dashboards", endpoint, response.Status)
	case http.StatusNotFound:
		return fmt.Errorf("dashboard: no dashboard with UID %q at %s (%s)",
			uid, endpoint, response.Status)
	default:
		// Grafana returns a JSON body with a message on most errors; a short
		// excerpt of it says more than the status alone.
		excerpt, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		detail := strings.TrimSpace(string(excerpt))
		if detail == "" {
			return fmt.Errorf("dashboard: %s returned %s", endpoint, response.Status)
		}
		return fmt.Errorf("dashboard: %s returned %s: %s", endpoint, response.Status, detail)
	}
}
