package backend

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsHandler serves /metrics, /healthz and /readyz on the metrics
// listener, which is bound to an internal interface and carries no auth.
func (s *Server) metricsHandler() http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.get() {
			http.Error(w, `{"status":"not ready"}`, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"status":"ready"}`))
	})
	return mux
}
