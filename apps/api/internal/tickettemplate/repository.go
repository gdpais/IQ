package tickettemplate

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Version struct {
	ID           string    `json:"id"`
	Version      int       `json:"version"`
	ScopeType    string    `json:"scope_type"`
	ScopeValue   string    `json:"scope_value"`
	TemplateYAML string    `json:"template_yaml"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(db *pgxpool.Pool) *Repository {
	return &Repository{db: db}
}

func LoadFile(path string) (string, Document, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", Document{}, nil, err
	}
	raw := string(data)
	doc, validationErrors, err := Validate(raw)
	return raw, doc, validationErrors, err
}

func (r *Repository) SaveDocument(ctx context.Context, raw string, doc Document, createdBy string) error {
	if strings.TrimSpace(createdBy) == "" {
		createdBy = "system"
	}

	if err := r.insertVersion(ctx, doc.Version, "global", "default", raw, createdBy); err != nil {
		return err
	}
	for _, override := range doc.Overrides {
		if err := r.insertVersion(ctx, doc.Version, strings.ToLower(override.Scope.Type), override.Scope.Value, raw, createdBy); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) ListVersions(ctx context.Context, limit int) ([]Version, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := r.db.Query(ctx, `
		SELECT id, version, scope_type, scope_value, template_yaml, created_by, created_at
		FROM ticket_template_versions
		ORDER BY created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []Version{}
	for rows.Next() {
		var item Version
		if err := rows.Scan(&item.ID, &item.Version, &item.ScopeType, &item.ScopeValue, &item.TemplateYAML, &item.CreatedBy, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *Repository) LatestGlobalTemplate(ctx context.Context) (string, error) {
	var raw string
	err := r.db.QueryRow(ctx, `
		SELECT template_yaml
		FROM ticket_template_versions
		WHERE scope_type = 'global'
		ORDER BY version DESC, created_at DESC
		LIMIT 1
	`).Scan(&raw)
	return raw, err
}

func (r *Repository) insertVersion(ctx context.Context, version int, scopeType string, scopeValue string, raw string, createdBy string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO ticket_template_versions (id, version, scope_type, scope_value, template_yaml, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,NOW())
	`, uuid.NewString(), version, scopeType, scopeValue, raw, createdBy)
	return err
}
