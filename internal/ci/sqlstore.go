package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/glebarez/sqlite" // pure Go: no cgo, like easyvcs' own driver
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SQLStore is the durable Store. It keeps the orchestrator's state in the SAME
// database the deployment already runs (sqlite / postgres / mysql), so a CI run
// survives a restart without introducing a second datastore.
//
// It deliberately uses its own tables (ci_*) and its own connection: easyvcs
// owns its schema and does not expose its handle.
type SQLStore struct {
	db *gorm.DB
}

// runRow is the persisted form of a Run. The nested job/step data is stored as a
// JSON blob: it is written as a whole after every stage and never queried by
// field, so normalising it into tables would add joins for no benefit.
type runRow struct {
	ID           string `gorm:"primaryKey"`
	Namespace    string `gorm:"index:idx_ci_run_ns;not null"`
	Workflow     string `gorm:"not null;default:''"`
	WorkflowPath string `gorm:"not null;default:''"`
	WorkflowSHA  string `gorm:"not null;default:''"`
	Trigger      string `gorm:"not null;default:''"`
	Ref          string `gorm:"not null;default:''"`
	Base         string `gorm:"not null;default:''"`
	Head         string `gorm:"not null;default:''"`
	Tag          string `gorm:"not null;default:''"`
	RR           int64  `gorm:"not null;default:0"`
	State        string `gorm:"not null;default:''"`
	Owner        string `gorm:"not null;default:''"`
	Actor        string `gorm:"not null;default:''"`
	GroupName    string `gorm:"column:group_name;index:idx_ci_run_group;not null;default:''"`
	Attempt      int    `gorm:"not null;default:0"`
	Steps        int    `gorm:"not null;default:0"`
	Created      int64  `gorm:"index:idx_ci_run_ns;not null"`
	Updated      int64  `gorm:"not null;default:0"`
	// Payload holds the jobs (with steps) and the changed paths as JSON.
	Payload []byte `gorm:"type:blob"`
}

func (runRow) TableName() string { return "ci_runs" }

// checkRow is the persisted form of a Check (one per rr+workflow).
type checkRow struct {
	ID        uint   `gorm:"primaryKey;autoIncrement"`
	Namespace string `gorm:"not null;uniqueIndex:idx_ci_check"`
	RR        int64  `gorm:"not null;uniqueIndex:idx_ci_check"`
	Name      string `gorm:"not null;uniqueIndex:idx_ci_check"`
	State     string `gorm:"not null;default:''"`
	Required  bool   `gorm:"not null;default:false"`
	Details   string `gorm:"not null;default:''"`
	RunID     string `gorm:"not null;default:''"`
	Job       string `gorm:"not null;default:''"`
	Updated   int64  `gorm:"not null;default:0"`
}

func (checkRow) TableName() string { return "ci_checks" }

// cancelRow records a cancellation request (a Run row keeps its last state so a
// GET is still meaningful after the process restarts).
type cancelRow struct {
	RunID     string `gorm:"primaryKey"`
	Requested int64  `gorm:"not null"`
}

func (cancelRow) TableName() string { return "ci_cancellations" }

// OpenStore opens (and migrates) the CI store.
//
// driver is "sqlite" | "postgres" | "mysql"; dsn is the connection string (for
// sqlite, a file path). An empty driver/dsn means "not configured", which the
// caller should treat as a reason to fall back to MemStore.
func OpenStore(driver, dsn string) (*SQLStore, error) {
	if driver == "" || dsn == "" {
		return nil, fmt.Errorf("ci: store driver/dsn is required")
	}
	var dial gorm.Dialector
	switch driver {
	case "sqlite", "sqlite3":
		// glebarez/sqlite is the pure-Go driver (modernc), so the platform keeps
		// building with CGO_ENABLED=0 — the same constraint easyvcs lives under.
		dial = sqlite.Open(dsn)
	case "postgres", "postgresql":
		dial = postgres.Open(dsn)
	case "mysql", "mariadb":
		dial = mysql.Open(dsn)
	default:
		return nil, fmt.Errorf("ci: unsupported store driver %q", driver)
	}
	db, err := gorm.Open(dial, &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("ci: open store: %w", err)
	}
	if err := db.AutoMigrate(&runRow{}, &checkRow{}, &cancelRow{}); err != nil {
		return nil, fmt.Errorf("ci: migrate: %w", err)
	}
	return &SQLStore{db: db}, nil
}

// NewSQLStore wraps an existing handle (tests).
func NewSQLStore(db *gorm.DB) (*SQLStore, error) {
	if err := db.AutoMigrate(&runRow{}, &checkRow{}, &cancelRow{}); err != nil {
		return nil, err
	}
	return &SQLStore{db: db}, nil
}

// runPayload is the JSON blob in a run row.
type runPayload struct {
	Jobs         []JobResult `json:"jobs"`
	ChangedPaths []string    `json:"changed_paths,omitempty"`
}

// PutRun upserts a run. The conflict target is the primary key (the run id), so
// a second Put updates rather than failing.
func (s *SQLStore) PutRun(ctx context.Context, r *Run) error {
	r.Updated = time.Now().UTC().UnixMilli()
	payload, err := json.Marshal(runPayload{Jobs: r.Jobs, ChangedPaths: r.ChangedPaths})
	if err != nil {
		return err
	}
	row := runRow{
		ID: r.ID, Namespace: r.Namespace, Workflow: r.Workflow,
		WorkflowPath: r.WorkflowPath, WorkflowSHA: r.WorkflowSHA,
		Trigger: string(r.Trigger), Ref: r.Ref, Base: r.Base, Head: r.Head, Tag: r.Tag,
		RR: r.RR, State: string(r.State), Owner: r.Owner, Actor: r.Actor,
		GroupName: r.Group,
		Attempt:   r.Attempt, Steps: r.Steps, Created: r.Created, Updated: r.Updated,
		Payload: payload,
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		UpdateAll: true,
	}).Create(&row).Error
}

// GetRun reads a run by id.
func (s *SQLStore) GetRun(ctx context.Context, id string) (*Run, error) {
	var row runRow
	err := s.db.WithContext(ctx).Where("id=?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return runFromRow(&row)
}

// ListRuns lists a namespace's runs, newest first.
func (s *SQLStore) ListRuns(ctx context.Context, namespace string, limit int) ([]*Run, error) {
	q := s.db.WithContext(ctx).Order("created DESC")
	if namespace != "" {
		q = q.Where("namespace=?", namespace)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	var rows []runRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Run, 0, len(rows))
	for i := range rows {
		r, err := runFromRow(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// PutCheck upserts a check.
//
// The conflict target is the (namespace, rr, name) UNIQUE index, not the
// auto-increment id: a second Put must UPDATE the existing row (there is one
// check per RR+workflow).
func (s *SQLStore) PutCheck(ctx context.Context, c *Check) error {
	c.Updated = time.Now().UTC().UnixMilli()
	row := checkRow{
		Namespace: c.Namespace, RR: c.RR, Name: c.Name, State: string(c.State),
		Required: c.Required, Details: c.Details, RunID: c.RunID, Job: c.Job,
		Updated: c.Updated,
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "namespace"}, {Name: "rr"}, {Name: "name"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"state", "required", "details", "run_id", "job", "updated",
		}),
	}).Create(&row).Error
}

// ListChecks lists the checks of one RR.
func (s *SQLStore) ListChecks(ctx context.Context, namespace string, rr int64) ([]*Check, error) {
	var rows []checkRow
	if err := s.db.WithContext(ctx).Where("namespace=? AND rr=?", namespace, rr).
		Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Check, 0, len(rows))
	for i := range rows {
		out = append(out, &Check{
			RR: rows[i].RR, Namespace: rows[i].Namespace, Name: rows[i].Name,
			State: RunState(rows[i].State), Required: rows[i].Required,
			Details: rows[i].Details, RunID: rows[i].RunID, Job: rows[i].Job,
			Updated: rows[i].Updated,
		})
	}
	return out, nil
}

// CancelRun records a cancellation request and flips the run's state so every
// replica observes it (the in-memory store cannot).
func (s *SQLStore) CancelRun(ctx context.Context, id string) error {
	var row runRow
	err := s.db.WithContext(ctx).Where("id=?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).
		Create(&cancelRow{RunID: id, Requested: time.Now().UTC().UnixMilli()}).Error; err != nil {
		return err
	}
	return s.db.WithContext(ctx).Model(&runRow{}).Where("id=?", id).
		Updates(map[string]any{"state": string(RunCanceled), "updated": time.Now().UTC().UnixMilli()}).Error
}

// IsCanceled reports whether a cancellation was requested.
func (s *SQLStore) IsCanceled(ctx context.Context, id string) bool {
	var n int64
	if err := s.db.WithContext(ctx).Model(&cancelRow{}).Where("run_id=?", id).Count(&n).Error; err != nil {
		return false
	}
	return n > 0
}

func runFromRow(row *runRow) (*Run, error) {
	var payload runPayload
	if len(row.Payload) > 0 {
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return nil, fmt.Errorf("ci: decode run %s: %w", row.ID, err)
		}
	}
	return &Run{
		ID: row.ID, Namespace: row.Namespace, Workflow: row.Workflow,
		WorkflowPath: row.WorkflowPath, WorkflowSHA: row.WorkflowSHA,
		Trigger: Trigger(row.Trigger), Ref: row.Ref, Base: row.Base, Head: row.Head,
		Tag: row.Tag, RR: row.RR, State: RunState(row.State), Owner: row.Owner,
		Actor: row.Actor, Group: row.GroupName, Attempt: row.Attempt, Steps: row.Steps,
		Created: row.Created, Updated: row.Updated,
		Jobs: payload.Jobs, ChangedPaths: payload.ChangedPaths,
	}, nil
}

// Cancelable is implemented by stores that can report a cancellation outside the
// orchestrator's process (SQLStore). The orchestrator falls back to the run's
// state when a store does not implement it.
type Cancelable interface {
	IsCanceled(ctx context.Context, id string) bool
}

// LiveRunInGroup implements GroupQuerier: it returns the id of a non-terminal
// run in the group (excluding excludeRunID), or "".
func (s *SQLStore) LiveRunInGroup(ctx context.Context, group, excludeRunID string) (string, error) {
	if group == "" {
		return "", nil
	}
	var rows []runRow
	if err := s.db.WithContext(ctx).
		Where("group_name=? AND id<>?", group, excludeRunID).
		Order("created DESC").Find(&rows).Error; err != nil {
		return "", err
	}
	for i := range rows {
		if !RunState(rows[i].State).Terminal() {
			return rows[i].ID, nil
		}
	}
	return "", nil
}
