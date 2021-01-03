// Package dbjobqueue implements the interfaces in package jobqueue backed by a
// PostreSQL database.
//
// Data is stored non-reduntantly. Any data structure necessary for efficient
// access (e.g., dependants) are kept in memory.
package dbjobqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgtype"
	"github.com/jackc/pgx"
	"github.com/jackc/pgx/v4/pgxpool"
)

const (
	sqlEnqueue          = `INSERT INTO jobs(id, type, args, queued_at) VALUES ($1, $2, $3, NOW())`
	sqlInsertDependency = `INSERT INTO job_dependencies VALUES ($1, $2)`
	sqlNotify           = `NOTIFY jobs`
	sqlListen           = `LISTEN jobs`
	sqlUnlisten         = `UNLISTEN jobs`
	sqlQueryJob         = `
		SELECT result, queued_at, started_at, finished_at, canceled
		FROM jobs 
		WHERE id = $1`
	sqlDequeue = `
		UPDATE jobs
		SET started_at = now()
		WHERE id = (
		  SELECT id
		  FROM ready_jobs
			  -- use ANY here, because "type in ()" doesn't work with bound parameters
			  -- literal syntax for this is '{"a", "b"}': https://www.postgresql.org/docs/13/arrays.html
		  WHERE type = ANY($1)
		  LIMIT 1
		  FOR UPDATE SKIP LOCKED
		)
		RETURNING id, type, args`
	sqlQueryDependencies = `
		SELECT dependency_id
		FROM job_dependencies
		WHERE job_id = $1`
	sqlFinishJob = `
		UPDATE jobs
		SET finished_at = now(), result = $1
		WHERE id = $2 and finished_at IS NULL`

	// set both started_at and finished_at to the same timestamp
	sqlCancelJob = `
		UPDATE jobs
		SET started_at = now(), finished_at = now(), canceled = TRUE
		WHERE id = $1 and finished_at IS NULL`
)

type dbJobQueue struct {
	pool *pgxpool.Pool
}

// On-disk job struct. Contains all necessary (but non-redundant) information
// about a job. These are not held in memory by the job queue, but
// (de)serialized on each access.
type job struct {
	Id           uuid.UUID       `json:"id"`
	Type         string          `json:"type"`
	Args         json.RawMessage `json:"args,omitempty"`
	Dependencies []uuid.UUID     `json:"dependencies"`
	Result       json.RawMessage `json:"result,omitempty"`

	QueuedAt   time.Time `json:"queued_at,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	Canceled bool `json:"canceled,omitempty"`
}

// Create a new dbJobQueue object for `url`.
func New(url string) (*dbJobQueue, error) {
	pool, err := pgxpool.Connect(context.Background(), url)
	if err != nil {
		return nil, fmt.Errorf("error establishing connection: %v", err)
	}

	return &dbJobQueue{pool}, nil
}

func (q *dbJobQueue) Close() {
	q.pool.Close()
}

func (q *dbJobQueue) Enqueue(jobType string, args interface{}, dependencies []uuid.UUID) (uuid.UUID, error) {
	conn, err := q.pool.Acquire(context.Background())
	if err != nil {
		return uuid.Nil, fmt.Errorf("error connecting to database: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(context.Background())
	if err != nil {
		return uuid.Nil, fmt.Errorf("error starting database transaction: %v", err)
	}
	defer tx.Rollback(context.Background()) // no-op if tx commits successfully

	id := uuid.New()
	_, err = conn.Exec(context.Background(), sqlEnqueue, id, jobType, args)
	if err != nil {
		return uuid.Nil, fmt.Errorf("error enqueuing job: %v", err)
	}

	for _, d := range dependencies {
		_, err = conn.Exec(context.Background(), sqlInsertDependency, id, d)
		if err != nil {
			return uuid.Nil, fmt.Errorf("error inserting dependency: %v", err)
		}
	}

	_, err = conn.Exec(context.Background(), sqlNotify)
	if err != nil {
		return uuid.Nil, fmt.Errorf("error notifying jobs channel: %v", err)
	}

	err = tx.Commit(context.Background())
	if err != nil {
		return uuid.Nil, fmt.Errorf("unable to commit database transaction: %v", err)
	}

	return id, nil
}

func (q *dbJobQueue) Dequeue(ctx context.Context, jobTypes []string) (uuid.UUID, []uuid.UUID, string, json.RawMessage, error) {
	// Return early if the context is already canceled.
	if err := ctx.Err(); err != nil {
		return uuid.Nil, nil, "", nil, err
	}

	conn, err := q.pool.Acquire(ctx)
	if err != nil {
		return uuid.Nil, nil, "", nil, fmt.Errorf("error connecting to database: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(ctx, sqlUnlisten) // ignore errors when cleaning up
		conn.Release()
	}()

	_, err = conn.Exec(ctx, sqlListen)
	if err != nil {
		return uuid.Nil, nil, "", nil, fmt.Errorf("error listening on jobs channel: %v", err)
	}

	var id uuid.UUID
	var jobType string
	var args json.RawMessage
	for {
		err = conn.QueryRow(ctx, sqlDequeue, jobTypes).Scan(&id, &jobType, &args)
		if err == nil {
			break
		}
		if err != nil && !errors.As(err, &pgx.ErrNoRows) {
			return uuid.Nil, nil, "", nil, fmt.Errorf("error dequeuing job: %v", err)
		}
		_, err = conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return uuid.Nil, nil, "", nil, fmt.Errorf("error waiting for notification on jobs channel: %v", err)
		}
	}

	dependencies, err := q.jobDependencies(ctx, conn, id)
	if err != nil {
		return uuid.Nil, nil, "", nil, fmt.Errorf("error querying the job's dependencies: %v", err)
	}

	return id, dependencies, jobType, args, nil
}

func (q *dbJobQueue) FinishJob(id uuid.UUID, result interface{}) error {
	conn, err := q.pool.Acquire(context.Background())
	if err != nil {
		return fmt.Errorf("error connecting to database: %v", err)
	}
	defer conn.Release()

	tag, err := conn.Exec(context.Background(), sqlFinishJob, result, id)
	if err != nil {
		return fmt.Errorf("error finishing job %s: %v", id, err)
	}

	if tag.RowsAffected() != 1 {
		return fmt.Errorf("error finishing job %s: job does not exist", id)
	}

	return nil
}

func (q *dbJobQueue) CancelJob(id uuid.UUID) error {
	conn, err := q.pool.Acquire(context.Background())
	if err != nil {
		return fmt.Errorf("error connecting to database: %v", err)
	}
	defer conn.Release()

	tag, err := conn.Exec(context.Background(), sqlCancelJob, id)
	if err != nil {
		return fmt.Errorf("error canceling job %s: %v", id, err)
	}

	if tag.RowsAffected() != 1 {
		return fmt.Errorf("error canceling job %s: job does not exist", id)
	}

	return nil
}

func (q *dbJobQueue) JobStatus(id uuid.UUID) (result json.RawMessage, queued, started, finished time.Time, canceled bool, deps []uuid.UUID, err error) {
	conn, err := q.pool.Acquire(context.Background())
	if err != nil {
		return
	}
	defer conn.Release()

	var sp, fp *time.Time
	var rp pgtype.JSON
	err = conn.QueryRow(context.Background(), sqlQueryJob, id).Scan(&rp, &queued, &sp, &fp, &canceled)
	if err != nil {
		return
	}
	if sp != nil {
		started = *sp
	}
	if fp != nil {
		finished = *fp
	}
	if rp.Status != pgtype.Null {
		result = rp.Bytes
	}

	deps, err = q.jobDependencies(context.Background(), conn, id)
	if err != nil {
		return
	}
	return
}

func (q *dbJobQueue) jobDependencies(ctx context.Context, conn *pgxpool.Conn, id uuid.UUID) ([]uuid.UUID, error) {
	rows, err := conn.Query(ctx, sqlQueryDependencies, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dependencies := []uuid.UUID{}
	for rows.Next() {
		var d uuid.UUID
		rows.Scan(&d)
		dependencies = append(dependencies, d)
	}
	if rows.Err() != nil {
		return nil, err
	}

	return dependencies, nil
}
