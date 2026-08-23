package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

type metricPointPayload struct {
	Attempt    uint32    `json:"attempt"`
	Step       *int64    `json:"step,omitempty"`
	Value      float64   `json:"value"`
	ObservedAt time.Time `json:"observed_at"`
}

type metricSeriesPayload struct {
	Definition domain.MetricDefinition `json:"definition"`
	Points     []metricPointPayload    `json:"points"`
}

type jobMetricsPayload struct {
	JobState domain.JobState       `json:"job_state"`
	Cursor   int64                 `json:"cursor"`
	Metrics  []metricSeriesPayload `json:"metrics"`
}

// consoleJobMetrics serves the job page's chart data under the browser
// session, exactly like the HTML console pages.
func (s *Server) consoleJobMetrics(response http.ResponseWriter, request *http.Request) {
	if _, _, err := s.browserSession(request); errors.Is(err, sqlitestore.ErrNotFound) {
		writeError(response, http.StatusUnauthorized, "unauthorized", "a signed-in browser session is required")
		return
	} else if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "the console is temporarily unavailable")
		return
	}
	s.writeJobMetrics(response, request)
}

func (s *Server) writeJobMetrics(response http.ResponseWriter, request *http.Request) {
	var after int64
	if raw := request.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(response, http.StatusBadRequest, "invalid_after", "after must be a cursor from a previous response")
			return
		}
		after = parsed
	}
	job, err := s.store.GetJob(request.Context(), request.PathValue("id"))
	if errors.Is(err, sqlitestore.ErrNotFound) {
		writeError(response, http.StatusNotFound, "not_found", "job not found")
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "the job could not be read")
		return
	}
	points, cursor, err := s.store.ListJobMetricPoints(request.Context(), job.ID, after)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "internal", "job metrics could not be read")
		return
	}

	// Declared series always appear so the page can render every panel up
	// front; automatic system series appear once they have reported samples.
	definitions := append([]domain.MetricDefinition{}, job.Spec.Metrics...)
	for _, definition := range domain.SystemMetricDefinitions() {
		for _, point := range points {
			if point.Name == definition.Name {
				definitions = append(definitions, definition)
				break
			}
		}
	}
	payload := jobMetricsPayload{JobState: job.State, Cursor: cursor, Metrics: make([]metricSeriesPayload, 0, len(definitions))}
	for _, definition := range definitions {
		series := metricSeriesPayload{Definition: definition, Points: make([]metricPointPayload, 0)}
		for _, point := range points {
			if point.Name != definition.Name {
				continue
			}
			series.Points = append(series.Points, metricPointPayload{
				Attempt: point.Attempt, Step: point.Step, Value: point.Value, ObservedAt: point.ObservedAt,
			})
		}
		payload.Metrics = append(payload.Metrics, series)
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, payload)
}
