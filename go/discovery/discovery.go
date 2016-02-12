// package fifonodupes manages a queue as described: an ordered
// queue with no duplicates.
// The configured processor function is called on an entry that's
// received and this happens in parallel until we reach maxCapacity
// go-routines are running in parallel.  Any new requests get queued
// and processed as a go routine becomes free.  The queue is processed
// in FIFO order.  If a request is received for an element that is
// already in the queue or being processed then it will be silently
// ignored.
package discovery

import (
	"errors"
	"sync"

	"github.com/outbrain/golib/log"
	"github.com/outbrain/orchestrator/go/inst"
)

type Queue struct {
	concurrency    uint                      // current concurrency
	done           chan inst.InstanceKey     // for synchronising completed discoveries
	inputChan      <-chan inst.InstanceKey   // input channel we are reading from
	knownKeys      map[inst.InstanceKey]bool // pending instances so we don't queue anything up more than one
	lock           sync.Mutex                // lock while making changes
	maxConcurrency uint                      // maximum concurrency of the queue
	processor      func(i inst.InstanceKey)  // process to run on each received key
	queue          []inst.InstanceKey        // instances in fifo order so we process in the order received.
}

var emptyKey = inst.InstanceKey{}

// provide a channel to read from and a function to run on the instance to be processed
func NewQueue(maxConcurrency uint, inputChan chan inst.InstanceKey, processor func(i inst.InstanceKey)) *Queue {
	log.Infof("Queue.NewQueue()")
	q := new(Queue)

	q.concurrency = 0                    // explicitly
	q.done = make(chan inst.InstanceKey) // Do I need this to be larger?
	q.inputChan = inputChan
	q.knownKeys = make(map[inst.InstanceKey]bool)
	q.maxConcurrency = maxConcurrency
	q.processor = processor
	q.queue = make([]inst.InstanceKey, 0)

	return q
}

// add the key to the slice if it does not exist in known keys
// - goroutine safe as only called inside the mutex
func (q *Queue) push(key inst.InstanceKey) {
	if key == emptyKey {
		log.Fatal("Queue.push(%v) is empty", key)
	}
	// log.Debugf("Queue.push(%+v)", key)

	if _, found := q.knownKeys[key]; !found {
		// log.Debugf("Queue.push() adding %+v to knownKeys", key)
		// add to the items that are being processed
		q.knownKeys[key] = true
		q.queue = append(q.queue, key)
	} else {
		// If key already there we just ignore it as the request is in the queue.
		// the known key also records stuff in the queue, so pending + active jobs.
		// log.Debugf("Queue.push() ignoring knownKey %+v", key)
	}
}

// remove the entry and remove it from known keys
func (q *Queue) pop() (inst.InstanceKey, error) {
	if len(q.queue) == 0 {
		return inst.InstanceKey{}, errors.New("q.pop() on empty queue")
	}
	key := q.queue[0]
	q.queue = q.queue[1:]
	delete(q.knownKeys, key)
	// log.Debugf("Queue.pop() returns %+v", key)
	return key, nil
}

// dispatch a job from the queue (assumes we are in a locked state)
func (q *Queue) dispatch() {
	key, err := q.pop() // should never give an error but let's check anyway
	if err != nil {
		log.Fatal("Queue.dispatch() q.pop() returns: %+v", err)
		return
	}
	if key == emptyKey {
		log.Fatal("Queue.dispatch() key is empty")
	}

	q.concurrency++
	q.knownKeys[key] = true

	// log.Debugf("Queue.dispatch() key: %q, concurrency: %d", key, q.concurrency)

	// dispatch a discoverInstance() but tell us when we're done (to limit concurrency)
	go func() { // discover asynchronously
		q.processor(key)
		q.done <- key
	}()
}

// acknowledge a job has finished
// - we deal with the locking inside
func (q *Queue) acknowledgeJob(key inst.InstanceKey) {
	q.lock.Lock()
	delete(q.knownKeys, key)
	q.concurrency--
	// log.Debugf("Queue.acknowledgeJob(%+v) q.concurrency: %d", key, q.concurrency)
	q.lock.Unlock()
}

// drain queue by dispatching any jobs we have still
func (q *Queue) maybeDispatch() {
	q.lock.Lock()
	// log.Debugf("Queue.maybeDispatch() q.concurrency: %d, q.maxConcurrency: %d, len(q.queue): %d", q.concurrency, q.maxConcurrency, len(q.queue))
	if q.concurrency < q.maxConcurrency && len(q.queue) > 0 {
		q.dispatch()
	}
	q.lock.Unlock()
}

// add an entry to the queue and dispatch something if concurrency is low enough
// - we deal with locking inside
func (q *Queue) queueAndMaybeDispatch(key inst.InstanceKey) {
	if key == emptyKey {
		log.Fatal("Queue.queueAndMaybeDispatch(%v) is empty", key)
	}
	q.lock.Lock()
	// log.Debugf("Queue.queueAndMaybeDispatch(%+v) concurency: %d", key, q.concurrency)
	q.push(key)
	if q.concurrency < q.maxConcurrency && len(q.queue) > 0 {
		q.dispatch()
	}
	q.lock.Unlock()
}

// cleanup is called when the input channel closes.
// we can not sit in the loop so we have to wait for running go-routines to finish
// but also to dispatch anything left in the queue until finally everything is done.
func (q *Queue) cleanup() {
	log.Infof("Queue.cleanup()")
	for q.concurrency > 0 && len(q.queue) > 0 {
		q.maybeDispatch()
		if key, closed := <-q.done; closed {
			return
		} else {
			q.acknowledgeJob(key)
		}
	}
}

// Ends when all elements in the queue have been handled.
// we read from inputChan and call processor up to maxConcurrency times in parallel
func (q *Queue) HandleRequests() {
	if q == nil {
		log.Infof("Queue.HandleRequests() q == nil ??. Should not happen")

		// no queue, nothing to do
		return
	}
	log.Infof("Queue.NewQueue() processing requests")
	for {
		select {
		case key, ok := <-q.inputChan:
			if ok {
				if key != emptyKey {
					q.queueAndMaybeDispatch(key)
				} else {
					log.Warningf("Queue.HandleRequests() q.inputChan received empty key %+v, ignoring (fix the upstream code to prevent this)", key)
				}
			} else {
				q.cleanup()
				log.Infof("Queue.HandleRequests() q.inputChan is closed. returning")
				return
			}
		case key, ok := <-q.done:
			if ok {
				q.acknowledgeJob(key)
			} else {
				log.Infof("Queue.HandleRequests() q.done is closed. returning (shouldn't get here)")
				return // we shouldn't get here as the return above should get triggered first
			}
		}
	}
}
