package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const deploymentIncidentWindow = 24 * time.Hour

var defaultSnapshotReportTypes = []string{"live", "sre", "dora", "executive"}

type Repository struct {
	db *pgxpool.Pool
}

type metricRow struct {
	ID                    string
	Title                 string
	Severity              string
	Service               string
	Environment           string
	OwnerTeam             string
	Status                string
	StartedAt             time.Time
	AcknowledgedAt        *time.Time
	ResolvedAt            *time.Time
	FirstAlertObservedAt  *time.Time
	AlertCount            int64
	ReopenCount           int64
	LinkedDeploymentCount int64
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func (r *Repository) LiveMetrics(ctx context.Context, filter Filter) (LiveMetrics, error) {
	rows, err := r.metricRows(ctx, filter)
	if err != nil {
		return LiveMetrics{}, err
	}
	return ComputeLiveMetrics(rows, time.Now().UTC()), nil
}

func (r *Repository) SREReport(ctx context.Context, filter Filter) (SREReport, error) {
	rows, err := r.metricRows(ctx, filter)
	if err != nil {
		return SREReport{}, err
	}
	return ComputeSREReport(rows, time.Now().UTC()), nil
}

func (r *Repository) DORAReport(ctx context.Context, filter Filter) (DORAReport, error) {
	rows, err := r.metricRows(ctx, filter)
	if err != nil {
		return DORAReport{}, err
	}
	deploymentCount, err := r.deploymentCount(ctx, filter)
	if err != nil {
		return DORAReport{}, err
	}
	return ComputeDORAReport(rows, deploymentCount, time.Now().UTC()), nil
}

func (r *Repository) ExecutiveReport(ctx context.Context, filter Filter) (ExecutiveReport, error) {
	rows, err := r.metricRows(ctx, filter)
	if err != nil {
		return ExecutiveReport{}, err
	}
	return ComputeExecutiveReport(rows, time.Now().UTC()), nil
}

func (r *Repository) IncidentCSVRows(ctx context.Context, filter Filter) ([]IncidentCSVRow, error) {
	rows, err := r.metricRows(ctx, filter)
	if err != nil {
		return nil, err
	}
	return IncidentCSVRowsFromMetrics(rows), nil
}

func (r *Repository) MaterializeSnapshots(ctx context.Context, req SnapshotRequest) ([]ReportSnapshot, error) {
	reportTypes, err := normalizeReportTypes(req.ReportTypes)
	if err != nil {
		return nil, err
	}

	rows, err := r.metricRows(ctx, req.Filter)
	if err != nil {
		return nil, err
	}
	deploymentCount := int64(0)
	for _, reportType := range reportTypes {
		if reportType == "dora" {
			deploymentCount, err = r.deploymentCount(ctx, req.Filter)
			if err != nil {
				return nil, err
			}
			break
		}
	}

	now := time.Now().UTC()
	dimensions := filterDimensions(req.Filter)
	snapshots := make([]ReportSnapshot, 0, len(reportTypes))
	for _, reportType := range reportTypes {
		payload, err := snapshotPayload(reportType, rows, deploymentCount, now)
		if err != nil {
			return nil, err
		}
		snapshot, err := r.insertSnapshot(ctx, reportType, dimensions, now, payload)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func (r *Repository) ListSnapshots(ctx context.Context, reportType string, limit int) ([]ReportSnapshot, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where := []string{}
	args := []any{}
	if reportType != "" {
		args = append(args, reportType)
		where = append(where, "report_type = $1")
	}
	args = append(args, limit)

	query := `
		SELECT id, report_type, dimensions, computed_at, payload, created_at
		FROM report_snapshots`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY computed_at DESC LIMIT $%d", len(args))

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	snapshots := []ReportSnapshot{}
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

func (r *Repository) insertSnapshot(ctx context.Context, reportType string, dimensions map[string]any, computedAt time.Time, payload []byte) (ReportSnapshot, error) {
	var dimensionsJSON []byte
	var payloadJSON []byte
	var out ReportSnapshot
	err := r.db.QueryRow(ctx, `
		INSERT INTO report_snapshots (id, report_type, dimensions, computed_at, payload, created_at)
		VALUES ($1,$2,$3::jsonb,$4,$5::jsonb,NOW())
		RETURNING id, report_type, dimensions, computed_at, payload, created_at
	`, uuid.NewString(), reportType, string(mapToJSON(dimensions)), computedAt, string(payload)).Scan(
		&out.ID, &out.ReportType, &dimensionsJSON, &out.ComputedAt, &payloadJSON, &out.CreatedAt,
	)
	if err != nil {
		return ReportSnapshot{}, err
	}
	out.Dimensions = jsonToMap(dimensionsJSON)
	out.Payload = payloadJSON
	return out, nil
}

type snapshotScanner interface {
	Scan(dest ...any) error
}

func scanSnapshot(row snapshotScanner) (ReportSnapshot, error) {
	var out ReportSnapshot
	var dimensions []byte
	var payload []byte
	if err := row.Scan(&out.ID, &out.ReportType, &dimensions, &out.ComputedAt, &payload, &out.CreatedAt); err != nil {
		return ReportSnapshot{}, err
	}
	out.Dimensions = jsonToMap(dimensions)
	out.Payload = payload
	return out, nil
}

func IncidentCSVRowsFromMetrics(rows []metricRow) []IncidentCSVRow {
	out := make([]IncidentCSVRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, IncidentCSVRow{
			ID:             row.ID,
			Title:          row.Title,
			Severity:       row.Severity,
			Service:        row.Service,
			Environment:    row.Environment,
			OwnerTeam:      row.OwnerTeam,
			Status:         row.Status,
			StartedAt:      row.StartedAt,
			AcknowledgedAt: row.AcknowledgedAt,
			ResolvedAt:     row.ResolvedAt,
			AlertCount:     row.AlertCount,
			ReopenCount:    row.ReopenCount,
		})
	}
	return out
}

func (r *Repository) IngestDeployment(ctx context.Context, req DeploymentRequest) (Deployment, error) {
	if strings.TrimSpace(req.Service) == "" {
		return Deployment{}, fmt.Errorf("service is required")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return Deployment{}, fmt.Errorf("environment is required")
	}
	if strings.TrimSpace(req.Version) == "" {
		return Deployment{}, fmt.Errorf("version is required")
	}
	deployedAt := time.Now().UTC()
	if req.DeployedAt != nil {
		deployedAt = req.DeployedAt.UTC()
	}
	if req.Source == "" {
		req.Source = "generic"
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}

	incidentID, err := r.findDeploymentLinkedIncident(ctx, req.Service, req.Environment, deployedAt)
	if err != nil {
		return Deployment{}, err
	}

	var incidentIDPtr *string
	if incidentID != "" {
		incidentIDPtr = &incidentID
	}
	var out Deployment
	var metadata []byte
	err = r.db.QueryRow(ctx, `
		INSERT INTO deployments (id, incident_id, service, environment, version, deployed_at, source, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NOW())
		RETURNING id, incident_id, service, environment, version, deployed_at, source, metadata, created_at
	`, uuid.NewString(), incidentIDPtr, req.Service, req.Environment, req.Version, deployedAt, req.Source, mapToJSON(req.Metadata)).Scan(
		&out.ID, &out.IncidentID, &out.Service, &out.Environment, &out.Version, &out.DeployedAt, &out.Source, &metadata, &out.CreatedAt,
	)
	if err != nil {
		return Deployment{}, err
	}
	out.Metadata = jsonToMap(metadata)
	return out, nil
}

func (r *Repository) findDeploymentLinkedIncident(ctx context.Context, service string, environment string, deployedAt time.Time) (string, error) {
	var incidentID string
	err := r.db.QueryRow(ctx, `
		SELECT id
		FROM incidents
		WHERE service = $1
		  AND environment = $2
		  AND started_at >= $3
		  AND started_at <= $4
		ORDER BY started_at ASC
		LIMIT 1
	`, service, environment, deployedAt, deployedAt.Add(deploymentIncidentWindow)).Scan(&incidentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return incidentID, nil
}

func (r *Repository) deploymentCount(ctx context.Context, filter Filter) (int64, error) {
	where := []string{}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if filter.Service != "" {
		add("service = $%d", filter.Service)
	}
	if filter.Environment != "" {
		add("environment = $%d", filter.Environment)
	}
	if filter.From != nil {
		add("deployed_at >= $%d", *filter.From)
	}
	if filter.To != nil {
		add("deployed_at < $%d", *filter.To)
	}

	query := "SELECT COUNT(*) FROM deployments"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	var count int64
	err := r.db.QueryRow(ctx, query, args...).Scan(&count)
	return count, err
}

func (r *Repository) metricRows(ctx context.Context, filter Filter) ([]metricRow, error) {
	where := []string{}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if filter.Severity != "" {
		add("i.severity = $%d", filter.Severity)
	}
	if filter.Service != "" {
		add("i.service = $%d", filter.Service)
	}
	if filter.Environment != "" {
		add("i.environment = $%d", filter.Environment)
	}
	if filter.OwnerTeam != "" {
		add("i.owner_team = $%d", filter.OwnerTeam)
	}
	if filter.Status != "" {
		add("i.status = $%d", filter.Status)
	}
	if filter.From != nil {
		add("i.started_at >= $%d", *filter.From)
	}
	if filter.To != nil {
		add("i.started_at < $%d", *filter.To)
	}

	query := `
		SELECT
			i.id, i.title, i.severity, i.service, i.environment, i.owner_team, i.status,
			i.started_at, i.acknowledged_at, i.resolved_at,
			MIN(a.observed_at) AS first_alert_observed_at,
			COUNT(DISTINCT a.id) AS alert_count,
			COUNT(DISTINCT e.id) FILTER (WHERE e.event_type = 'incident_reopened') AS reopen_count,
			COUNT(DISTINCT d.id) AS linked_deployment_count
		FROM incidents i
		LEFT JOIN alerts a ON a.incident_id = i.id
		LEFT JOIN incident_events e ON e.incident_id = i.id
		LEFT JOIN deployments d ON d.incident_id = i.id`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += `
		GROUP BY i.id
		ORDER BY i.started_at DESC`

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []metricRow{}
	for rows.Next() {
		var row metricRow
		if err := rows.Scan(
			&row.ID, &row.Title, &row.Severity, &row.Service, &row.Environment, &row.OwnerTeam, &row.Status,
			&row.StartedAt, &row.AcknowledgedAt, &row.ResolvedAt, &row.FirstAlertObservedAt,
			&row.AlertCount, &row.ReopenCount, &row.LinkedDeploymentCount,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func ComputeLiveMetrics(rows []metricRow, now time.Time) LiveMetrics {
	metrics := LiveMetrics{
		CountsBySeverity:    map[string]int64{},
		CountsByService:     map[string]int64{},
		CountsByEnvironment: map[string]int64{},
		CountsByTeam:        map[string]int64{},
		CountsByStatus:      map[string]int64{},
	}

	mttd := []float64{}
	mtta := []float64{}
	mttr := []float64{}
	for _, row := range rows {
		metrics.IncidentCount++
		metrics.AlertCount += row.AlertCount
		metrics.ReopenCount += row.ReopenCount
		metrics.CountsBySeverity[row.Severity]++
		metrics.CountsByService[row.Service]++
		metrics.CountsByEnvironment[row.Environment]++
		metrics.CountsByTeam[row.OwnerTeam]++
		metrics.CountsByStatus[row.Status]++
		switch row.Status {
		case "open":
			metrics.OpenIncidentCount++
		case "acknowledged":
			metrics.AcknowledgedIncidentCount++
		case "resolved":
			metrics.ResolvedIncidentCount++
		}
		if row.AlertCount > 0 {
			metrics.IncidentsWithAlerts++
		}
		if row.FirstAlertObservedAt != nil {
			mttd = append(mttd, nonNegativeSeconds(row.StartedAt.Sub(*row.FirstAlertObservedAt)))
		}
		if row.AcknowledgedAt != nil {
			mtta = append(mtta, nonNegativeSeconds(row.AcknowledgedAt.Sub(row.StartedAt)))
		}
		if row.ResolvedAt != nil {
			mttr = append(mttr, nonNegativeSeconds(row.ResolvedAt.Sub(row.StartedAt)))
		}
		if row.Status != "resolved" {
			age := now.Sub(row.StartedAt)
			if isSLAAtRisk(row.Severity, age) {
				metrics.SLAAtRiskCount++
			}
			if isSLABreached(row.Severity, age) {
				metrics.SLABreachCount++
			}
		}
	}
	metrics.AlertToIncidentConversion = ratio(metrics.IncidentsWithAlerts, metrics.AlertCount)
	metrics.DuplicateSuppressionCount = metrics.AlertCount - metrics.IncidentsWithAlerts
	if metrics.DuplicateSuppressionCount < 0 {
		metrics.DuplicateSuppressionCount = 0
	}
	metrics.ReopenRate = ratio(metrics.ReopenCount, metrics.IncidentCount)
	metrics.MeanTimeToDetectSeconds = averagePtr(mttd)
	metrics.MeanTimeToAckSeconds = averagePtr(mtta)
	metrics.MeanTimeToResolveSeconds = averagePtr(mttr)
	return metrics
}

func ComputeSREReport(rows []metricRow, now time.Time) SREReport {
	return SREReport{LiveMetrics: ComputeLiveMetrics(rows, now), GeneratedAt: now}
}

func ComputeDORAReport(rows []metricRow, deploymentCount int64, now time.Time) DORAReport {
	var linked int64
	restoreDurations := []float64{}
	for _, row := range rows {
		if row.LinkedDeploymentCount > 0 {
			linked++
			if row.ResolvedAt != nil {
				restoreDurations = append(restoreDurations, row.ResolvedAt.Sub(row.StartedAt).Seconds())
			}
		}
	}

	return DORAReport{
		DeploymentCount:             deploymentCount,
		DeploymentLinkedIncidents:   linked,
		ChangeFailureRate:           ratio(linked, deploymentCount),
		TimeToRestoreServiceSeconds: averagePtr(restoreDurations),
		GeneratedAt:                 now,
	}
}

func ComputeExecutiveReport(rows []metricRow, now time.Time) ExecutiveReport {
	metrics := ComputeLiveMetrics(rows, now)
	var downtime float64
	notes := []string{}
	for _, row := range rows {
		end := now
		if row.ResolvedAt != nil {
			end = *row.ResolvedAt
		}
		if end.After(row.StartedAt) {
			downtime += end.Sub(row.StartedAt).Seconds()
		}
		if row.Status != "resolved" && (row.Severity == "critical" || row.Severity == "high") {
			notes = append(notes, fmt.Sprintf("%s %s incident remains %s", row.Severity, row.Service, row.Status))
		}
	}
	return ExecutiveReport{
		IncidentCount:       metrics.IncidentCount,
		DowntimeSeconds:     downtime,
		SLAAtRiskCount:      metrics.SLAAtRiskCount,
		SLABreachCount:      metrics.SLABreachCount,
		SeverityTrends:      metrics.CountsBySeverity,
		ServiceReliability:  metrics.CountsByService,
		BusinessImpactNotes: notes,
		GeneratedAt:         now,
	}
}

func normalizeReportTypes(values []string) ([]string, error) {
	if len(values) == 0 {
		return append([]string{}, defaultSnapshotReportTypes...), nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		reportType := strings.ToLower(strings.TrimSpace(value))
		if reportType == "" || seen[reportType] {
			continue
		}
		switch reportType {
		case "live", "sre", "dora", "executive":
			seen[reportType] = true
			out = append(out, reportType)
		default:
			return nil, fmt.Errorf("unsupported report type %q", value)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one report type is required")
	}
	return out, nil
}

func snapshotPayload(reportType string, rows []metricRow, deploymentCount int64, now time.Time) ([]byte, error) {
	var payload any
	switch reportType {
	case "live":
		payload = ComputeLiveMetrics(rows, now)
	case "sre":
		payload = ComputeSREReport(rows, now)
	case "dora":
		payload = ComputeDORAReport(rows, deploymentCount, now)
	case "executive":
		payload = ComputeExecutiveReport(rows, now)
	default:
		return nil, fmt.Errorf("unsupported report type %q", reportType)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func filterDimensions(filter Filter) map[string]any {
	out := map[string]any{}
	if filter.Severity != "" {
		out["severity"] = filter.Severity
	}
	if filter.Service != "" {
		out["service"] = filter.Service
	}
	if filter.Environment != "" {
		out["environment"] = filter.Environment
	}
	if filter.OwnerTeam != "" {
		out["owner_team"] = filter.OwnerTeam
	}
	if filter.Status != "" {
		out["status"] = filter.Status
	}
	if filter.From != nil {
		out["from"] = filter.From.UTC().Format(time.RFC3339)
	}
	if filter.To != nil {
		out["to"] = filter.To.UTC().Format(time.RFC3339)
	}
	return out
}

func isSLAAtRisk(severity string, age time.Duration) bool {
	return age >= slaThreshold(severity)/2
}

func isSLABreached(severity string, age time.Duration) bool {
	return age >= slaThreshold(severity)
}

func slaThreshold(severity string) time.Duration {
	switch strings.ToLower(severity) {
	case "critical":
		return time.Hour
	case "high":
		return 4 * time.Hour
	case "medium":
		return 24 * time.Hour
	default:
		return 72 * time.Hour
	}
}

func nonNegativeSeconds(duration time.Duration) float64 {
	if duration < 0 {
		return 0
	}
	return duration.Seconds()
}

func averagePtr(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	var total float64
	for _, value := range values {
		total += value
	}
	avg := total / float64(len(values))
	return &avg
}

func ratio(numerator int64, denominator int64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func mapToJSON(payload map[string]any) []byte {
	data, _ := json.Marshal(payload)
	return data
}

func jsonToMap(data []byte) map[string]any {
	if len(data) == 0 {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{}
	}
	return out
}
