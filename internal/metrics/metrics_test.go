package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestMetrics(t *testing.T) {
	m := New("1.2.3")
	m.DeployStarted("app")
	m.DeployStarted("app")

	out := scrape(t, m)
	if !strings.Contains(out, `pushbot_deploy_in_progress{project="app"} 2`) {
		t.Errorf("in-progress gauge missing:\n%s", out)
	}

	m.DeployFinished("app", "success", 3*time.Second)
	m.DeployFinished("app", "timeout", 900*time.Second)
	m.RequestRejected(401)
	m.RequestRejected(401)
	m.RequestRejected(403)

	out = scrape(t, m)
	for _, want := range []string{
		`pushbot_build_info{version="1.2.3"} 1`,
		`pushbot_deploys_total{project="app",result="success"} 1`,
		`pushbot_deploys_total{project="app",result="timeout"} 1`,
		`pushbot_deploy_in_progress{project="app"} 0`,
		`pushbot_deploy_duration_seconds_count{project="app"} 2`,
		`pushbot_deploy_duration_seconds_bucket{project="app",le="5"} 1`,
		`pushbot_last_deploy_success{project="app"} 0`,
		`pushbot_last_deploy_timestamp_seconds{project="app"}`,
		`pushbot_requests_rejected_total{code="401"} 2`,
		`pushbot_requests_rejected_total{code="403"} 1`,
		`go_goroutines `,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}
