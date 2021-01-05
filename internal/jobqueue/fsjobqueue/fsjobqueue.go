// Package fsjobqueue implements a filesystem-backed job queue. It implements
// the interfaces in package jobqueue.
//
// Jobs are stored in the file system, using the `jsondb` package. However,
// this package does not use the file system as a database, but keeps some
// state in memory. This means that access to a given directory must be
// exclusive to only one `fsJobQueue` object at a time. A single `fsJobQueue`
// can be safely accessed from multiple goroutines, though.
//
// Data is stored non-reduntantly. Any data structure necessary for efficient
// access (e.g., dependants) are kept in memory.
package fsjobqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/osbuild/osbuild-composer/internal/jobqueue"
	"github.com/osbuild/osbuild-composer/internal/jsondb"
)

type fsJobQueue struct {
	// Protects all fields of this struct. In particular, it ensures
	// transactions on `db` are atomic. All public functions except
	// JobStatus hold it while they're running. Dequeue() releases it
	// briefly while waiting on pending channels.
	mu sync.Mutex

	db *jsondb.JSONDatabase

	// Maps job types to channels of job ids for that type.
	pending map[string]chan uuid.UUID

	// Maps job ids to the jobs that depend on it, if any of those
	// dependants have not yet finished.
	dependants map[uuid.UUID][]uuid.UUID
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

// The size of channels used in fsJobQueue for queueing jobs.
// Note that each job type has its own queue.
const channelSize = 100

// Create a new fsJobQueue object for `dir`. This object must have exclusive
// access to `dir`. If `dir` contains jobs created from previous runs, they are
// loaded and rescheduled to run if necessary.
func New(dir string) (*fsJobQueue, error) {
	q := &fsJobQueue{
		db:         jsondb.New(dir, 0600),
		pending:    make(map[string]chan uuid.UUID),
		dependants: make(map[uuid.UUID][]uuid.UUID),
	}

	// Look for jobs that are still pending and build the dependant map.
	ids, err := q.db.List()
	if err != nil {
		return nil, fmt.Errorf("error listing jobs: %v", err)
	}
	for _, id := range ids {
		uuid, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("invalid job '%s' in db: %v", id, err)
		}
		j, err := q.readJob(uuid)
		if err != nil {
			return nil, err
		}
		err = q.maybeEnqueue(j, true)
		if err != nil {
			return nil, err
		}
	}

	return q, nil
}

func (q *fsJobQueue) Enqueue(jobType string, args interface{}, dependencies []uuid.UUID) (uuid.UUID, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	var j = job{
		Id:           uuid.New(),
		Type:         jobType,
		Dependencies: dependencies,
		QueuedAt:     time.Now(),
	}

	var err error
	j.Args, err = json.Marshal(args)
	if err != nil {
		return uuid.Nil, fmt.Errorf("error marshaling job arguments: %v", err)
	}

	// Verify dependendencies early, so that the job doesn't get written
	// when one of them doesn't exist.
	for _, d := range j.Dependencies {
		exists, err := q.db.Read(d.String(), nil)
		if err != nil {
			return uuid.Nil, err
		}
		if !exists {
			return uuid.Nil, jobqueue.ErrNotExist
		}
	}

	// Write the job before updating in-memory state, so that the latter
	// doesn't become corrupt when writing fails.
	err = q.db.Write(j.Id.String(), j)
	if err != nil {
		return uuid.Nil, fmt.Errorf("cannot write job: %v:", err)
	}

	err = q.maybeEnqueue(&j, true)
	if err != nil {
		return uuid.Nil, err
	}

	return j.Id, nil
}

func (q *fsJobQueue) Dequeue(ctx context.Context, jobTypes []string) (uuid.UUID, []uuid.UUID, string, json.RawMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Return early if the context is already canceled.
	if err := ctx.Err(); err != nil {
		return uuid.Nil, nil, "", nil, err
	}

	// Filter q.pending by the `jobTypes`. Ignore those job types that this
	// queue doesn't accept.
	chans := []chan uuid.UUID{}
	for _, jt := range jobTypes {
		c, exists := q.pending[jt]
		if !exists {
			c = make(chan uuid.UUID, channelSize)
			q.pending[jt] = c
		}
		chans = append(chans, c)
	}

	// Loop until finding a non-canceled job.
	var j *job
	for {
		// Unlock the mutex while polling channels, so that multiple goroutines
		// can wait at the same time.
		q.mu.Unlock()
		id, err := selectUUIDChannel(ctx, chans)
		q.mu.Lock()

		// Delete empty channels
		for _, jt := range jobTypes {
			c, exists := q.pending[jt]
			if exists && len(c) == 0 {
				close(c)
				delete(q.pending, jt)
			}
		}

		if err != nil {
			return uuid.Nil, nil, "", nil, err
		}

		j, err = q.readJob(id)
		if err != nil {
			return uuid.Nil, nil, "", nil, err
		}

		if !j.Canceled {
			break
		}
	}

	j.StartedAt = time.Now()

	err := q.db.Write(j.Id.String(), j)
	if err != nil {
		return uuid.Nil, nil, "", nil, fmt.Errorf("error writing job %s: %v", j.Id, err)
	}

	return j.Id, j.Dependencies, j.Type, j.Args, nil
}

func (q *fsJobQueue) FinishJob(id uuid.UUID, result interface{}) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	j, err := q.readJob(id)
	if err != nil {
		return err
	}

	if j.Canceled {
		return jobqueue.ErrCanceled
	}

	if j.StartedAt.IsZero() || !j.FinishedAt.IsZero() {
		return jobqueue.ErrNotRunning
	}

	j.FinishedAt = time.Now()

	j.Result, err = json.Marshal(result)
	if err != nil {
		return fmt.Errorf("error marshaling result: %v", err)
	}

	// Write before notifying dependants, because it will be read again.
	err = q.db.Write(id.String(), j)
	if err != nil {
		return fmt.Errorf("error writing job %s: %v", id, err)
	}

	for _, depid := range q.dependants[id] {
		dep, err := q.readJob(depid)
		if err != nil {
			return err
		}
		err = q.maybeEnqueue(dep, false)
		if err != nil {
			return err
		}
	}
	delete(q.dependants, id)

	return nil
}

func (q *fsJobQueue) CancelJob(id uuid.UUID) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	j, err := q.readJob(id)
	if err != nil {
		return err
	}

	if !j.FinishedAt.IsZero() {
		return jobqueue.ErrNotRunning
	}

	j.Canceled = true

	err = q.db.Write(id.String(), j)
	if err != nil {
		return fmt.Errorf("error writing job %s: %v", id, err)
	}

	return nil
}

func (q *fsJobQueue) JobStatus(id uuid.UUID) (result json.RawMessage, queued, started, finished time.Time, canceled bool, deps []uuid.UUID, err error) {
	j, err := q.readJob(id)
	if err != nil {
		return
	}

	result = j.Result
	queued = j.QueuedAt
	started = j.StartedAt
	finished = j.FinishedAt
	canceled = j.Canceled
	deps = j.Dependencies

	return
}

// Reads job with `id`. This is a thin wrapper around `q.db.Read`, which
// returns the job directly, or and error if a job with `id` does not exist.
func (q *fsJobQueue) readJob(id uuid.UUID) (*job, error) {
	var j job
	exists, err := q.db.Read(id.String(), &j)
	if err != nil {
		return nil, fmt.Errorf("error reading job '%s': %v", id, err)
	}
	if !exists {
		// return corrupt database?
		return nil, jobqueue.ErrNotExist
	}
	return &j, nil
}

// Enqueue `job` if it is pending and all its dependencies have finished.
// Update `q.dependants` if the job was not queued and updateDependants is true
// (i.e., when this is a new job).
// `q.mu` must be locked when this method is called. The only exception is
// `New()` because no concurrent calls are possible there.
func (q *fsJobQueue) maybeEnqueue(j *job, updateDependants bool) error {
	if !j.StartedAt.IsZero() {
		return nil
	}

	depsFinished := true
	for _, id := range j.Dependencies {
		j, err := q.readJob(id)
		if err != nil {
			return err
		}
		if j.FinishedAt.IsZero() {
			depsFinished = false
			break
		}
	}

	if depsFinished {
		c, exists := q.pending[j.Type]
		if !exists {
			c = make(chan uuid.UUID, channelSize)
			q.pending[j.Type] = c
		}
		c <- j.Id
	} else if updateDependants {
		for _, id := range j.Dependencies {
			q.dependants[id] = append(q.dependants[id], j.Id)
		}
	}

	return nil
}

// Select on a list of `chan uuid.UUID`s. Returns an error if one of the
// channels is closed.
//
// Uses reflect.Select(), because the `select` statement cannot operate on an
// unknown amount of channels.
func selectUUIDChannel(ctx context.Context, chans []chan uuid.UUID) (uuid.UUID, error) {
	cases := []reflect.SelectCase{
		{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(ctx.Done()),
		},
	}
	for _, c := range chans {
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(c),
		})
	}

	chosen, value, recvOK := reflect.Select(cases)
	if !recvOK {
		if chosen == 0 {
			return uuid.Nil, ctx.Err()
		} else {
			return uuid.Nil, errors.New("channel was closed unexpectedly")
		}
	}

	return value.Interface().(uuid.UUID), nil
}
