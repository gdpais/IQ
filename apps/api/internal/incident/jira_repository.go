package incident

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (r *Repository) UpsertJIRALink(ctx context.Context, incidentID string, issueKey string, issueID string) (JIRALink, error) {
	var issueIDPtr *string
	if issueID != "" {
		issueIDPtr = &issueID
	}

	var out JIRALink
	err := r.db.QueryRow(ctx, `
		INSERT INTO jira_links (incident_id, jira_issue_key, jira_issue_id, created_at, updated_at)
		VALUES ($1,$2,$3,NOW(),NOW())
		ON CONFLICT (incident_id) DO UPDATE
		SET jira_issue_key = EXCLUDED.jira_issue_key,
		    jira_issue_id = EXCLUDED.jira_issue_id,
		    updated_at = NOW()
		RETURNING incident_id, jira_issue_key, jira_issue_id, created_at, updated_at
	`, incidentID, issueKey, issueIDPtr).Scan(&out.IncidentID, &out.JIRAIssueKey, &out.JIRAIssueID, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

func (r *Repository) FindJIRALink(ctx context.Context, incidentID string) (*JIRALink, error) {
	link, err := r.GetJIRALink(ctx, incidentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return link, err
}
