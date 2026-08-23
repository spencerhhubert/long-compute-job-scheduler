package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// JobMetricPoint is one recorded sample of a job metric series, tagged with
// the attempt that produced it.
type JobMetricPoint struct {
	Name       string
	Attempt    uint32
	Step       *int64
	Value      float64
	ObservedAt time.Time
}

// ListJobMetricPoints returns every metric sample for a job recorded after the
// given cursor, in commit order, together with the cursor for the next call.
// A zero cursor reads the full history; passing the returned cursor back reads
// only samples committed since, so a page can poll cheaply while a job runs.
func (s *Store) ListJobMetricPoints(ctx context.Context, jobID string, after int64) ([]JobMetricPoint, int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.rowid, a.attempt_number, m.name, m.value, m.step, m.observed_at
		FROM metric_samples AS m JOIN attempts AS a ON a.id = m.attempt_id
		WHERE a.job_id = ? AND m.rowid > ?
		ORDER BY m.rowid
	`, jobID, after)
	if err != nil {
		return nil, 0, fmt.Errorf("list job metric points: %w", err)
	}
	defer rows.Close()
	points := make([]JobMetricPoint, 0)
	cursor := after
	for rows.Next() {
		var point JobMetricPoint
		var step sql.NullInt64
		var observed string
		if err := rows.Scan(&cursor, &point.Attempt, &point.Name, &point.Value, &step, &observed); err != nil {
			return nil, 0, err
		}
		if step.Valid {
			value := step.Int64
			point.Step = &value
		}
		if point.ObservedAt, err = time.Parse(time.RFC3339Nano, observed); err != nil {
			return nil, 0, err
		}
		points = append(points, point)
	}
	return points, cursor, rows.Err()
}
