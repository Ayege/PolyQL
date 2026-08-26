package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// grafanaResponse is the envelope a real Grafana wraps a dashboard in.
const grafanaResponse = `{
  "meta": {"slug": "api-overview", "url": "/d/abc123/api-overview"},
  "dashboard": {
    "uid": "abc123",
    "title": "API overview",
    "panels": [
      {"id": 1, "title": "Request rate",
       "targets": [{"refId": "A", "expr": "rate(http_requests_total[5m])"}]}
    ]
  }
}`

// newGrafana starts a stub Grafana and returns it with the client pointed at it.
func newGrafana(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *GrafanaClient) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, &GrafanaClient{BaseURL: server.URL, HTTP: server.Client()}
}

func TestGrafanaFetchesADashboard(t *testing.T) {
	var gotPath, gotAuth, gotAccept string
	server, client := newGrafana(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAccept = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(grafanaResponse))
	})
	_ = server
	client.Token = "glsa_test"

	dash, err := client.Dashboard(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}

	if want := "/api/dashboards/uid/abc123"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if want := "Bearer glsa_test"; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}

	// The dashboard has to come back unwrapped from its envelope, or every
	// panel walk below finds nothing.
	if len(dash.Panels) != 1 {
		t.Fatalf("got %d panels, want 1", len(dash.Panels))
	}
	if got := dash.Panels[0].Title; got != "Request rate" {
		t.Errorf("panel title = %q", got)
	}
}

// TestGrafanaFetchedDashboardTranslates is the point of the whole feature: a
// dashboard pulled over HTTP has to go through the same translator as one read
// from disk, with no second parsing path to drift.
func TestGrafanaFetchedDashboardTranslates(t *testing.T) {
	_, client := newGrafana(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(grafanaResponse))
	})

	dash, err := client.Dashboard(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}

	reg := testRegistry(t)
	result, err := Translate(dash, "promql", "logql", reg)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(result.PanelReports) != 1 {
		t.Fatalf("got %d panel reports, want 1", len(result.PanelReports))
	}
	if got := result.PanelReports[0].TranslatedExpr; !strings.Contains(got, "rate(") {
		t.Errorf("translated expression = %q, want a rate", got)
	}
}

func TestGrafanaURLConstruction(t *testing.T) {
	cases := []struct {
		base, uid, want string
	}{
		{"https://grafana.example.com", "abc", "https://grafana.example.com/api/dashboards/uid/abc"},
		// A trailing slash is what a copy-paste from a browser gives you.
		{"https://grafana.example.com/", "abc", "https://grafana.example.com/api/dashboards/uid/abc"},
		// Grafana behind a sub-path is a common reverse-proxy layout.
		{"https://example.com/grafana", "abc", "https://example.com/grafana/api/dashboards/uid/abc"},
		{"http://localhost:3000", "abc", "http://localhost:3000/api/dashboards/uid/abc"},
		// A UID is user input and reaches a URL path, so it must be escaped.
		{"https://g.example.com", "a/b", "https://g.example.com/api/dashboards/uid/a%2Fb"},
	}

	for _, c := range cases {
		t.Run(c.base+" "+c.uid, func(t *testing.T) {
			client := &GrafanaClient{BaseURL: c.base}
			got, err := client.dashboardURL(c.uid)
			if err != nil {
				t.Fatalf("dashboardURL: %v", err)
			}
			if got != c.want {
				t.Errorf("= %q, want %q", got, c.want)
			}
		})
	}

	t.Run("a bare host is rejected with advice", func(t *testing.T) {
		// "grafana.example.com" with no scheme parses as a relative path, which
		// would otherwise produce a request to nowhere.
		client := &GrafanaClient{BaseURL: "grafana.example.com"}
		_, err := client.dashboardURL("abc")
		if err == nil {
			t.Fatal("expected an error for a URL with no scheme")
		}
		if !strings.Contains(err.Error(), "https://") {
			t.Errorf("error %q should show the expected form", err)
		}
	})
}

func TestGrafanaErrors(t *testing.T) {
	t.Run("401 with no token names the environment variable", func(t *testing.T) {
		_, client := newGrafana(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		_, err := client.Dashboard(context.Background(), "abc")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "GRAFANA_TOKEN") {
			t.Errorf("error %q should say how to authenticate", err)
		}
	})

	t.Run("401 with a token blames the token", func(t *testing.T) {
		_, client := newGrafana(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		})
		client.Token = "stale"
		_, err := client.Dashboard(context.Background(), "abc")
		if err == nil {
			t.Fatal("expected an error")
		}
		// Telling someone to set a token they already set is the unhelpful
		// version of this message.
		if strings.Contains(err.Error(), "set GRAFANA_TOKEN") {
			t.Errorf("error %q should not ask for a token that was sent", err)
		}
		if !strings.Contains(err.Error(), "refused the token") {
			t.Errorf("error %q should blame the token", err)
		}
	})

	t.Run("404 names the uid", func(t *testing.T) {
		_, client := newGrafana(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		_, err := client.Dashboard(context.Background(), "nosuch")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "nosuch") {
			t.Errorf("error %q should name the uid", err)
		}
	})

	t.Run("another status carries the body", func(t *testing.T) {
		_, client := newGrafana(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"database is locked"}`))
		})
		_, err := client.Dashboard(context.Background(), "abc")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "database is locked") {
			t.Errorf("error %q should carry Grafana's own message", err)
		}
	})

	t.Run("a 200 that is not this API is caught", func(t *testing.T) {
		// Pointing at a dashboard page rather than the Grafana root returns
		// HTML or an unrelated JSON document with a 200.
		_, client := newGrafana(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"meta":{}}`))
		})
		_, err := client.Dashboard(context.Background(), "abc")
		if err == nil {
			t.Fatal("expected an error for a response with no dashboard")
		}
		if !strings.Contains(err.Error(), "Grafana root") {
			t.Errorf("error %q should say what to check", err)
		}
	})

	t.Run("malformed JSON is reported as such", func(t *testing.T) {
		_, client := newGrafana(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json at all`))
		})
		_, err := client.Dashboard(context.Background(), "abc")
		if err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("empty inputs are refused before any request", func(t *testing.T) {
		if _, err := (&GrafanaClient{}).Dashboard(context.Background(), "abc"); err == nil {
			t.Error("expected an error for a missing base URL")
		}
		if _, err := (&GrafanaClient{BaseURL: "http://x"}).Dashboard(context.Background(), " "); err == nil {
			t.Error("expected an error for a missing uid")
		}
	})
}

// TestGrafanaHonoursContext covers the cancellation path, which is what stops a
// hung Grafana hanging the CLI.
func TestGrafanaHonoursContext(t *testing.T) {
	_, client := newGrafana(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(grafanaResponse))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.Dashboard(ctx, "abc"); err == nil {
		t.Error("expected the canceled context to abort the fetch")
	}
}

// TestParseDashboardSharesOnePath pins that the HTTP and file routes decode
// through the same function, so a change to one cannot leave the other behind.
func TestParseDashboardSharesOnePath(t *testing.T) {
	var envelope struct {
		Dashboard json.RawMessage `json:"dashboard"`
	}
	if err := json.Unmarshal([]byte(grafanaResponse), &envelope); err != nil {
		t.Fatal(err)
	}

	fromAPI, err := ParseDashboard(envelope.Dashboard, "http://example/api")
	if err != nil {
		t.Fatalf("ParseDashboard: %v", err)
	}
	if len(fromAPI.Panels) != 1 {
		t.Errorf("got %d panels, want 1", len(fromAPI.Panels))
	}

	t.Run("the source is named in a parse error", func(t *testing.T) {
		_, err := ParseDashboard([]byte("{{{"), "http://example/api/dashboards/uid/abc")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "http://example/api/dashboards/uid/abc") {
			t.Errorf("error %q should name where the bytes came from", err)
		}
	})
}
