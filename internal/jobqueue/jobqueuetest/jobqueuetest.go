// Package jobqueuetest provides test functions to verify a JobQueue
// implementation satisfies the interface in package jobqueue.

package jobqueuetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/osbuild/osbuild-composer/internal/jobqueue"
)

type MakeJobQueue func() (q jobqueue.JobQueue, stop func(), err error)

func TestJobQueue(t *testing.T, makeJobQueue MakeJobQueue) {
	wrap := func(f func(t *testing.T, q jobqueue.JobQueue)) func(*testing.T) {
		q, stop, err := makeJobQueue()
		require.NoError(t, err)
		return func(t *testing.T) {
			defer stop() // use defer because f() might call testing.T.FailNow()
			f(t, q)
		}
	}

	t.Run("errors", wrap(testErrors))
	t.Run("args", wrap(testArgs))
	t.Run("cancel", wrap(testCancel))
	t.Run("job-types", wrap(testJobTypes))
	t.Run("dependencies", wrap(testDependencies))
	t.Run("multiple-workers", wrap(testMultipleWorkers))
}

func testErrors(t *testing.T, q jobqueue.JobQueue) {
	// not serializable to JSON
	id, err := q.Enqueue("test", make(chan string), nil)
	require.Error(t, err)
	require.Equal(t, uuid.Nil, id)

	// invalid dependency
	id, err = q.Enqueue("test", "arg0", []uuid.UUID{uuid.New()})
	require.Error(t, err)
	require.Equal(t, uuid.Nil, id)
}

func testArgs(t *testing.T, q jobqueue.JobQueue) {
	type argument struct {
		I int
		S string
	}

	oneargs := argument{7, "🐠"}
	one := pushTestJob(t, q, "fish", oneargs, nil)

	twoargs := argument{42, "🐙"}
	two := pushTestJob(t, q, "octopus", twoargs, nil)

	var parsedArgs argument

	id, deps, typ, args, err := q.Dequeue(context.Background(), []string{"octopus"})
	require.NoError(t, err)
	require.Equal(t, two, id)
	require.Empty(t, deps)
	require.Equal(t, "octopus", typ)
	err = json.Unmarshal(args, &parsedArgs)
	require.NoError(t, err)
	require.Equal(t, twoargs, parsedArgs)

	id, deps, typ, args, err = q.Dequeue(context.Background(), []string{"fish"})
	require.NoError(t, err)
	require.Equal(t, one, id)
	require.Empty(t, deps)
	require.Equal(t, "fish", typ)
	err = json.Unmarshal(args, &parsedArgs)
	require.NoError(t, err)
	require.Equal(t, oneargs, parsedArgs)
}

func testJobTypes(t *testing.T, q jobqueue.JobQueue) {
	type testResult struct{}

	one := pushTestJob(t, q, "octopus", nil, nil)
	two := pushTestJob(t, q, "clownfish", nil, nil)

	require.Equal(t, two, finishNextTestJob(t, q, "clownfish", testResult{}, nil))
	require.Equal(t, one, finishNextTestJob(t, q, "octopus", testResult{}, nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id, deps, typ, args, err := q.Dequeue(ctx, []string{"zebra"})
	require.Equal(t, err, context.Canceled)
	require.Equal(t, uuid.Nil, id)
	require.Empty(t, deps)
	require.Equal(t, "", typ)
	require.Nil(t, args)
}

func testDependencies(t *testing.T, q jobqueue.JobQueue) {
	type testResult struct{}

	t.Run("done-before-pushing-dependant", func(t *testing.T) {
		one := pushTestJob(t, q, "test", nil, nil)
		two := pushTestJob(t, q, "test", nil, nil)

		r := []uuid.UUID{}
		r = append(r, finishNextTestJob(t, q, "test", testResult{}, nil))
		r = append(r, finishNextTestJob(t, q, "test", testResult{}, nil))
		require.ElementsMatch(t, []uuid.UUID{one, two}, r)

		j := pushTestJob(t, q, "test", nil, []uuid.UUID{one, two})
		_, queued, started, finished, canceled, deps, err := q.JobStatus(j)
		require.NoError(t, err)
		require.True(t, !queued.IsZero())
		require.True(t, started.IsZero())
		require.True(t, finished.IsZero())
		require.False(t, canceled)
		require.ElementsMatch(t, deps, []uuid.UUID{one, two})

		require.Equal(t, j, finishNextTestJob(t, q, "test", testResult{}, []uuid.UUID{one, two}))

		result, queued, started, finished, canceled, deps, err := q.JobStatus(j)
		require.NoError(t, err)
		require.True(t, !queued.IsZero())
		require.True(t, !started.IsZero())
		require.True(t, !finished.IsZero())
		require.False(t, canceled)
		require.ElementsMatch(t, deps, []uuid.UUID{one, two})

		err = json.Unmarshal(result, &testResult{})
		require.NoError(t, err)
	})

	t.Run("done-after-pushing-dependant", func(t *testing.T) {
		one := pushTestJob(t, q, "test", nil, nil)
		two := pushTestJob(t, q, "test", nil, nil)

		j := pushTestJob(t, q, "test", nil, []uuid.UUID{one, two})
		_, queued, started, finished, canceled, deps, err := q.JobStatus(j)
		require.NoError(t, err)
		require.True(t, !queued.IsZero())
		require.True(t, started.IsZero())
		require.True(t, finished.IsZero())
		require.False(t, canceled)
		require.ElementsMatch(t, deps, []uuid.UUID{one, two})

		r := []uuid.UUID{}
		r = append(r, finishNextTestJob(t, q, "test", testResult{}, nil))
		r = append(r, finishNextTestJob(t, q, "test", testResult{}, nil))
		require.ElementsMatch(t, []uuid.UUID{one, two}, r)

		require.Equal(t, j, finishNextTestJob(t, q, "test", testResult{}, []uuid.UUID{one, two}))

		result, queued, started, finished, canceled, deps, err := q.JobStatus(j)
		require.NoError(t, err)
		require.True(t, !queued.IsZero())
		require.True(t, !started.IsZero())
		require.True(t, !finished.IsZero())
		require.False(t, canceled)
		require.ElementsMatch(t, deps, []uuid.UUID{one, two})

		err = json.Unmarshal(result, &testResult{})
		require.NoError(t, err)
	})
}

// Test that a job queue allows parallel access to multiple workers, mainly to
// verify the quirky unlocking in Dequeue().
func testMultipleWorkers(t *testing.T, q jobqueue.JobQueue) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		id, deps, typ, args, err := q.Dequeue(ctx, []string{"octopus"})
		require.NoError(t, err)
		require.NotEmpty(t, id)
		require.Empty(t, deps)
		require.Equal(t, "octopus", typ)
		require.Equal(t, json.RawMessage("null"), args)
	}()

	// Increase the likelihood that the above goroutine was scheduled and
	// is waiting in Dequeue().
	time.Sleep(10 * time.Millisecond)

	// This call to Dequeue() should not block on the one in the goroutine.
	id := pushTestJob(t, q, "clownfish", nil, nil)
	r, deps, typ, args, err := q.Dequeue(context.Background(), []string{"clownfish"})
	require.NoError(t, err)
	require.Equal(t, id, r)
	require.Empty(t, deps)
	require.Equal(t, "clownfish", typ)
	require.Equal(t, json.RawMessage("null"), args)

	// Now wake up the Dequeue() in the goroutine and wait for it to finish.
	_ = pushTestJob(t, q, "octopus", nil, nil)
	<-done
}

func testCancel(t *testing.T, q jobqueue.JobQueue) {
	type testResult struct{}

	// Cancel a non-existing job
	err := q.CancelJob(uuid.New())
	require.Error(t, err)

	// Cancel a pending job
	id := pushTestJob(t, q, "clownfish", nil, nil)
	require.NotEmpty(t, id)
	err = q.CancelJob(id)
	require.NoError(t, err)
	result, _, _, _, canceled, _, err := q.JobStatus(id)
	require.NoError(t, err)
	require.True(t, canceled)
	require.Nil(t, result)
	err = q.FinishJob(id, &testResult{})
	require.Error(t, err)

	// Cancel a running job, which should not dequeue the canceled job from above
	id = pushTestJob(t, q, "clownfish", nil, nil)
	require.NotEmpty(t, id)
	r, deps, typ, args, err := q.Dequeue(context.Background(), []string{"clownfish"})
	require.NoError(t, err)
	require.Equal(t, id, r)
	require.Empty(t, deps)
	require.Equal(t, "clownfish", typ)
	require.Equal(t, json.RawMessage("null"), args)
	err = q.CancelJob(id)
	require.NoError(t, err)
	result, _, _, _, canceled, _, err = q.JobStatus(id)
	require.NoError(t, err)
	require.True(t, canceled)
	require.Nil(t, result)
	err = q.FinishJob(id, &testResult{})
	require.Error(t, err)

	// Cancel a finished job, which is a no-op
	id = pushTestJob(t, q, "clownfish", nil, nil)
	require.NotEmpty(t, id)
	r, deps, typ, args, err = q.Dequeue(context.Background(), []string{"clownfish"})
	require.NoError(t, err)
	require.Equal(t, id, r)
	require.Empty(t, deps)
	require.Equal(t, "clownfish", typ)
	require.Equal(t, json.RawMessage("null"), args)
	err = q.FinishJob(id, &testResult{})
	require.NoError(t, err)
	err = q.CancelJob(id)
	require.NoError(t, err)
	result, _, _, _, canceled, _, err = q.JobStatus(id)
	require.NoError(t, err)
	require.False(t, canceled)
	err = json.Unmarshal(result, &testResult{})
	require.NoError(t, err)
}

func pushTestJob(t *testing.T, q jobqueue.JobQueue, jobType string, args interface{}, dependencies []uuid.UUID) uuid.UUID {
	t.Helper()
	id, err := q.Enqueue(jobType, args, dependencies)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	return id
}

func finishNextTestJob(t *testing.T, q jobqueue.JobQueue, jobType string, result interface{}, deps []uuid.UUID) uuid.UUID {
	id, d, typ, args, err := q.Dequeue(context.Background(), []string{jobType})
	require.NoError(t, err)
	require.NotEmpty(t, id)
	require.ElementsMatch(t, deps, d)
	require.Equal(t, jobType, typ)
	require.NotNil(t, args)

	err = q.FinishJob(id, result)
	require.NoError(t, err)

	return id
}
