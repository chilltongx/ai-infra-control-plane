package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
)

func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	s.metrics.mu.Lock()
	inFlight := s.metrics.inFlight
	panics := s.metrics.panics
	requests := make(map[metricKey]metricValue, len(s.metrics.requests))
	for key, value := range s.metrics.requests {
		requests[key] = value
	}
	s.metrics.mu.Unlock()

	keys := make([]metricKey, 0, len(requests))
	for key := range requests {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].route != keys[j].route {
			return keys[i].route < keys[j].route
		}
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].status < keys[j].status
	})

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintln(w, "# HELP controlplane_http_requests_total Total HTTP requests.")
	_, _ = fmt.Fprintln(w, "# TYPE controlplane_http_requests_total counter")
	for _, key := range keys {
		value := requests[key]
		_, _ = fmt.Fprintf(w, "controlplane_http_requests_total{method=%s,route=%s,status=%s} %d\n", quote(key.method), quote(key.route), quote(strconv.Itoa(key.status)), value.count)
	}
	_, _ = fmt.Fprintln(w, "# HELP controlplane_http_request_duration_seconds_sum Total HTTP request time.")
	_, _ = fmt.Fprintln(w, "# TYPE controlplane_http_request_duration_seconds_sum counter")
	for _, key := range keys {
		value := requests[key]
		_, _ = fmt.Fprintf(w, "controlplane_http_request_duration_seconds_sum{method=%s,route=%s,status=%s} %.9f\n", quote(key.method), quote(key.route), quote(strconv.Itoa(key.status)), value.duration)
	}
	_, _ = fmt.Fprintln(w, "# HELP controlplane_http_requests_in_flight Current HTTP requests.")
	_, _ = fmt.Fprintln(w, "# TYPE controlplane_http_requests_in_flight gauge")
	_, _ = fmt.Fprintf(w, "controlplane_http_requests_in_flight %d\n", inFlight)
	_, _ = fmt.Fprintln(w, "# HELP controlplane_http_panics_total Recovered HTTP panics.")
	_, _ = fmt.Fprintln(w, "# TYPE controlplane_http_panics_total counter")
	_, _ = fmt.Fprintf(w, "controlplane_http_panics_total %d\n", panics)
}

func quote(value string) string { return strconv.Quote(value) }
