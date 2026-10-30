package dbuf

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rec struct{ P, I int }

// sink collects every batch it is given.
type sink[T any] struct {
	mu      sync.Mutex
	batches [][]T
}

func (s *sink[T]) write(b []T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]T(nil), b...))
	return nil
}

func (s *sink[T]) all() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []T
	for _, b := range s.batches {
		out = append(out, b...)
	}
	return out
}

// Every record written by concurrent producers arrives exactly once, and each
// producer's records arrive in the order it put them.
func TestNoLossNoDuplicateOrderPerProducer(t *testing.T) {
	var s sink[rec]
	w := New(s.write, 64)
	const producers, each = 10, 1000
	var wg sync.WaitGroup
	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := w.Put(rec{p, i}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	last := make([]int, producers)
	for i := range last {
		last[i] = -1
	}
	out := s.all()
	for _, r := range out {
		if r.I != last[r.P]+1 {
			t.Fatalf("producer %d: got %d after %d", r.P, r.I, last[r.P])
		}
		last[r.P] = r.I
	}
	if len(out) != producers*each {
		t.Fatalf("records out = %d, want %d", len(out), producers*each)
	}
	st := w.Stats()
	if st.Records != producers*each || st.MaxBatch != 64 || st.Batches != int64((producers*each+63)/64) {
		t.Fatalf("stats %+v", st)
	}
}

// The point of the double buffer: while the flusher is stuck writing one full
// buffer, producers fill the other one with no wait. They wait only when both
// buffers are full.
func TestProducerFillsSpareWhileFlusherWrites(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	w := New(func([]int) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}, 10)
	for i := range 10 {
		w.Put(i) // the 10th put hands buffer A to the flusher
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("flusher never started writing")
	}
	// Buffer A is mid-write. Fill 9 of buffer B: none of these may wait.
	done := make(chan struct{})
	go func() {
		for i := range 9 {
			w.Put(100 + i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("producer waited while the spare buffer had room")
	}
	if s := w.Stats(); s.ProducerWaits != 0 {
		t.Fatalf("producer waits = %d, want 0", s.ProducerWaits)
	}
	// The 10th record fills B while A is still being written: now the producer must wait.
	blocked := make(chan struct{})
	go func() { w.Put(109); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("producer kept going with both buffers full")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("producer never resumed after the flusher freed a buffer")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	s := w.Stats()
	if s.ProducerWaits != 1 || s.Records != 20 || s.Batches != 2 {
		t.Fatalf("stats %+v, want 1 wait, 20 records, 2 batches", s)
	}
}

// Sync returns only after every earlier record is written, partial buffer included.
func TestSyncWaitsForEveryEarlierRecord(t *testing.T) {
	var written atomic.Int64
	w := New(func(b []int) error {
		time.Sleep(20 * time.Millisecond)
		written.Add(int64(len(b)))
		return nil
	}, 10)
	defer w.Close()
	for i := range 25 {
		w.Put(i)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if written.Load() != 25 {
		t.Fatalf("Sync returned with %d of 25 written", written.Load())
	}
	if err := w.Sync(); err != nil { // nothing pending: returns at once
		t.Fatal(err)
	}
}

// A failed batch is reported by the next Sync, once, and counted.
func TestSyncReportsErrorsOnce(t *testing.T) {
	boom := errors.New("disk full")
	var fail atomic.Bool
	fail.Store(true)
	w := New(func([]int) error {
		if fail.Load() {
			return boom
		}
		return nil
	}, 2)
	defer w.Close()
	w.Put(1)
	if err := w.Sync(); !errors.Is(err, boom) {
		t.Fatalf("Sync = %v, want the write error", err)
	}
	fail.Store(false)
	w.Put(2)
	if err := w.Sync(); err != nil {
		t.Fatalf("second Sync = %v, want nil (the error was already reported)", err)
	}
	if s := w.Stats(); s.FailedBatches != 1 || s.Records != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestCloseFlushesPartialAndRefusesLaterPuts(t *testing.T) {
	var s sink[int]
	w := New(s.write, 100)
	for i := range 7 {
		w.Put(i)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(s.all()); got != 7 {
		t.Fatalf("records = %d, want 7", got)
	}
	if err := w.Put(8); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close = %v, want ErrClosed", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

// singleLock is the design the double buffer replaces: the batch is written while
// the producers' lock is held.
type singleLock struct {
	mu  sync.Mutex
	buf []int
	cap int
	w   func()
}

func (s *singleLock) Put(v int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, v)
	if len(s.buf) == s.cap {
		s.w()
		s.buf = s.buf[:0]
	}
}

// With a producer and a writer that each take time, the double buffer overlaps them
// and the single lock adds them. The margin is wide so a loaded machine still passes.
func TestOverlapBeatsSingleLock(t *testing.T) {
	const capN, batches = 10, 8
	const writeDelay, workPerRec = 30 * time.Millisecond, 3 * time.Millisecond

	start := time.Now()
	sl := &singleLock{cap: capN, w: func() { time.Sleep(writeDelay) }}
	for i := range capN * batches {
		time.Sleep(workPerRec)
		sl.Put(i)
	}
	single := time.Since(start)

	start = time.Now()
	w := New(func([]int) error { time.Sleep(writeDelay); return nil }, capN)
	for i := range capN * batches {
		time.Sleep(workPerRec)
		w.Put(i)
	}
	w.Close()
	double := time.Since(start)

	t.Logf("single lock %v, double buffer %v (%.0f%%)", single, double, 100*float64(double)/float64(single))
	if double >= single*85/100 {
		t.Fatalf("double buffer %v is not clearly faster than single lock %v", double, single)
	}
}
