// Package dbuf is a double-buffered batch writer. Producers append records to the
// active buffer under a short mutex. When the buffer fills, it goes to one flusher
// goroutine and the producer continues on the spare buffer. The slow write (one
// database transaction per buffer) never runs under the producers' lock, and the
// database sees one writer, so producers never contend with it for a lock. A
// producer waits only when BOTH buffers are full, and that wait is counted.
package dbuf

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Stats is a snapshot of the writer's counters.
type Stats struct {
	Capacity       int   `json:"capacity"`
	Records        int64 `json:"records"`
	Batches        int64 `json:"batches"`
	FailedBatches  int64 `json:"failed_batches"`
	MaxBatch       int64 `json:"max_batch"`
	ProducerWaits  int64 `json:"producer_waits"`
	ProducerWaitNs int64 `json:"producer_wait_ns"`
	WriteNs        int64 `json:"write_ns"`
}

// ErrClosed is returned by Put after Close.
var ErrClosed = errors.New("dbuf: closed")

// Writer double-buffers records of type T and hands each full buffer to flush.
type Writer[T any] struct {
	mu     sync.Mutex
	cap    int
	active []T
	spare  chan []T // the free buffer; empty while the flusher holds it
	ready  chan []T // the buffer being written
	done   chan struct{}
	closed bool
	handed int64 // buffers handed to the flusher, under mu
	flushF func([]T) error

	doneMu  sync.Mutex
	doneC   *sync.Cond
	written int64 // buffers the flusher has finished, under doneMu
	errs    []error

	records, batches, failed, maxBatch, waits, waitNs, writeNs atomic.Int64
}

// New starts a writer whose two buffers each hold capacity records. flush is called
// from one goroutine only, once per buffer, in order.
func New[T any](flush func([]T) error, capacity int) *Writer[T] {
	if capacity < 1 {
		capacity = 1
	}
	d := &Writer[T]{
		cap:    capacity,
		active: make([]T, 0, capacity),
		spare:  make(chan []T, 1),
		ready:  make(chan []T, 1),
		done:   make(chan struct{}),
		flushF: flush,
	}
	d.doneC = sync.NewCond(&d.doneMu)
	d.spare <- make([]T, 0, capacity)
	go d.flush()
	return d
}

// Cap is the number of records in one buffer: the size of one write batch.
func (d *Writer[T]) Cap() int { return d.cap }

// Put appends one record. It blocks only while both buffers are full.
func (d *Writer[T]) Put(rec T) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrClosed
	}
	d.active = append(d.active, rec)
	if len(d.active) == d.cap {
		d.swapLocked()
	}
	return nil
}

// Flush hands a partly filled buffer to the flusher now, and returns at once.
func (d *Writer[T]) Flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed && len(d.active) > 0 {
		d.swapLocked()
	}
}

// Sync flushes and waits until every record put before the call is written. It
// returns the write errors seen since the previous Sync.
func (d *Writer[T]) Sync() error {
	d.mu.Lock()
	if !d.closed && len(d.active) > 0 {
		d.swapLocked()
	}
	target := d.handed
	d.mu.Unlock()
	d.doneMu.Lock()
	defer d.doneMu.Unlock()
	for d.written < target {
		d.doneC.Wait()
	}
	err := errors.Join(d.errs...)
	d.errs = nil
	return err
}

func (d *Writer[T]) swapLocked() {
	var next []T
	select {
	case next = <-d.spare:
	default:
		start := time.Now()
		next = <-d.spare
		d.waits.Add(1)
		d.waitNs.Add(int64(time.Since(start)))
	}
	d.ready <- d.active
	d.handed++
	d.active = next
}

func (d *Writer[T]) flush() {
	defer close(d.done)
	for batch := range d.ready {
		start := time.Now()
		err := d.flushF(batch)
		d.writeNs.Add(int64(time.Since(start)))
		if err != nil {
			d.failed.Add(1)
		} else {
			d.records.Add(int64(len(batch)))
		}
		d.batches.Add(1)
		for {
			m := d.maxBatch.Load()
			if int64(len(batch)) <= m || d.maxBatch.CompareAndSwap(m, int64(len(batch))) {
				break
			}
		}
		clear(batch)
		d.spare <- batch[:0]
		d.doneMu.Lock()
		d.written++
		if err != nil {
			d.errs = append(d.errs, err)
		}
		d.doneC.Broadcast()
		d.doneMu.Unlock()
	}
}

// Close writes what is left, stops the flusher, and returns unreported write errors.
func (d *Writer[T]) Close() error {
	d.mu.Lock()
	if !d.closed {
		if len(d.active) > 0 {
			d.swapLocked()
		}
		d.closed = true
		close(d.ready)
	}
	d.mu.Unlock()
	<-d.done
	d.doneMu.Lock()
	defer d.doneMu.Unlock()
	err := errors.Join(d.errs...)
	d.errs = nil
	return err
}

// Stats returns the counters.
func (d *Writer[T]) Stats() Stats {
	return Stats{
		Capacity: d.cap, Records: d.records.Load(), Batches: d.batches.Load(), FailedBatches: d.failed.Load(),
		MaxBatch: d.maxBatch.Load(), ProducerWaits: d.waits.Load(), ProducerWaitNs: d.waitNs.Load(),
		WriteNs: d.writeNs.Load(),
	}
}
