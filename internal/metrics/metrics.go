// Package metrics exposes deploy and request metrics in the Prometheus format.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds pushbot's collectors in a private registry.
type Metrics struct {
	registry      *prometheus.Registry
	deploys       *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	inProgress    *prometheus.GaugeVec
	lastSuccess   *prometheus.GaugeVec
	lastTimestamp *prometheus.GaugeVec
	rejected      *prometheus.CounterVec
}

// New creates the metrics, including Go runtime and process collectors.
func New(version string) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		deploys: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pushbot_deploys_total",
			Help: "Finished deploys by project and result (success, failed, timeout).",
		}, []string{"project", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pushbot_deploy_duration_seconds",
			Help:    "Deploy duration, including waiting for a previous deploy of the project.",
			Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600, 900, 1800},
		}, []string{"project"}),
		inProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pushbot_deploy_in_progress",
			Help: "Deploys of the project currently running or waiting for the project lock.",
		}, []string{"project"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pushbot_last_deploy_success",
			Help: "1 if the last finished deploy of the project succeeded, 0 otherwise.",
		}, []string{"project"}),
		lastTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pushbot_last_deploy_timestamp_seconds",
			Help: "Unix time when the last deploy of the project finished.",
		}, []string{"project"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pushbot_requests_rejected_total",
			Help: "Deploy requests rejected before the deploy started, by HTTP status code.",
		}, []string{"code"}),
	}
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pushbot_build_info",
		Help: "Always 1; the version label is the pushbot version.",
	}, []string{"version"})
	buildInfo.WithLabelValues(version).Set(1)

	m.registry.MustRegister(
		m.deploys, m.duration, m.inProgress, m.lastSuccess, m.lastTimestamp, m.rejected, buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// DeployStarted is called when a deploy starts running.
func (m *Metrics) DeployStarted(project string) {
	m.inProgress.WithLabelValues(project).Inc()
}

// DeployFinished is called when a deploy ends; result is success, failed or timeout.
func (m *Metrics) DeployFinished(project, result string, duration time.Duration) {
	m.inProgress.WithLabelValues(project).Dec()
	m.deploys.WithLabelValues(project, result).Inc()
	m.duration.WithLabelValues(project).Observe(duration.Seconds())
	success := 0.0
	if result == "success" {
		success = 1
	}
	m.lastSuccess.WithLabelValues(project).Set(success)
	m.lastTimestamp.WithLabelValues(project).Set(float64(time.Now().Unix()))
}

// RequestRejected is called when a deploy request fails before the deploy starts.
func (m *Metrics) RequestRejected(code int) {
	m.rejected.WithLabelValues(strconv.Itoa(code)).Inc()
}
