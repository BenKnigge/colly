package queue

import (
	"bytes"
	"encoding/json"
	"github.com/gocolly/colly/v2"
	"net/url"
	"sync"
)

const stop = true

// Storage is the interface of the queue's storage backend
// Storage must be concurrently safe for multiple goroutines.
type Storage interface {
	// Init initializes the storage
	Init() error
	// AddRequest adds a serialized request to the queue
	AddRequest([]byte) error
	// GetRequest pops the next serialized request from the queue
	// or returns error if the queue is empty
	GetRequest() ([]byte, error)
	// QueueSize returns with the size of the queue
	QueueSize() (int, error)
}

// Queue is a request queue which uses a Collector to consume
// requests in multiple threads
type Queue struct {
	// Threads defines the number of consumer threads
	Threads int
	storage Storage
	wake    chan struct{}
	mut     sync.Mutex // guards wake
}

// InMemoryQueueStorage is the default implementation of the Storage interface.
// InMemoryQueueStorage holds the request queue in memory.
type InMemoryQueueStorage struct {
	// MaxSize defines the capacity of the queue.
	// New requests are discarded if the queue size reaches MaxSize
	MaxSize int
	lock    *sync.RWMutex
	size    int
	first   *inMemoryQueueItem
	last    *inMemoryQueueItem
}

type inMemoryQueueItem struct {
	Request *colly.Request
	Next    *inMemoryQueueItem
}

// New creates a new queue with a Storage specified in argument
// A standard InMemoryQueueStorage is used if Storage argument is nil.
func New(threads int, s Storage) (*Queue, error) {
	if s == nil {
		s = &InMemoryQueueStorage{MaxSize: 100000}
	}
	if err := s.Init(); err != nil {
		return nil, err
	}
	return &Queue{
		Threads: threads,
		storage: s,
	}, nil
}

// IsEmpty returns true if the queue is empty
func (q *Queue) IsEmpty() bool {
	s, _ := q.Size()
	return s == 0
}

// AddURL adds a new URL to the queue
func (q *Queue) AddURL(URL string) error {
	u, err := url.Parse(URL)
	if err != nil {
		return err
	}
	r := &colly.Request{
		URL:    u,
		Method: "GET",
	}
	switch q.storage.(type) {
	case *InMemoryQueueStorage:
		return q.storage.(*InMemoryQueueStorage).AddRequestPointer(r)
	default:
		d, err := r.Marshal()
		if err != nil {
			return err
		}
		return q.storage.AddRequest(d)
	}

}

// AddRequest adds a new Request to the queue
func (q *Queue) AddRequest(r *colly.Request) error {
	q.mut.Lock()
	waken := q.wake != nil
	q.mut.Unlock()
	if !waken {
		return q.storeRequest(r)
	}
	err := q.storeRequest(r)
	if err != nil {
		return err
	}
	q.wake <- struct{}{}
	return nil
}

func (q *Queue) storeRequest(r *colly.Request) error {
	switch q.storage.(type) {
	case *InMemoryQueueStorage:
		return q.storage.(*InMemoryQueueStorage).AddRequestPointer(r)
	default:
		d, err := r.Marshal()
		if err != nil {
			return err
		}
		return q.storage.AddRequest(d)
	}
}

// Size returns the size of the queue
func (q *Queue) Size() (int, error) {
	return q.storage.QueueSize()
}

// Run starts consumer threads and calls the Collector
// to perform requests. Run blocks while the queue has active requests
// The given Storage must not be used directly while Run blocks.
func (q *Queue) Run(c *colly.Collector) error {
	q.mut.Lock()
	if q.wake != nil {
		q.mut.Unlock()
		panic("cannot call duplicate Queue.Run")
	}
	q.wake = make(chan struct{})
	q.mut.Unlock()

	requestc := make(chan *colly.Request)
	complete, errc := make(chan struct{}), make(chan error, 1)
	for i := 0; i < q.Threads; i++ {
		go independentRunner(requestc, complete)
	}
	go q.loop(c, requestc, complete, errc)
	defer close(requestc)
	return <-errc
}

func (q *Queue) loop(c *colly.Collector, requestc chan<- *colly.Request, complete <-chan struct{}, errc chan<- error) {
	var active int
	for {
		size, err := q.storage.QueueSize()
		if err != nil {
			errc <- err
			break
		}
		if size == 0 && active == 0 {
			// Terminate when
			//   1. No active requests
			//   2. Emtpy queue
			errc <- nil
			break
		}
		sent := requestc
		var req *colly.Request
		if size > 0 {
			req, err = q.loadRequest(c)
			if err != nil {
				// ignore an error returned by GetRequest() or
				// UnmarshalRequest()
				continue
			}
		} else {
			sent = nil
		}
	Sent:
		for {
			select {
			case sent <- req:
				active++
				break Sent
			case <-q.wake:
				if sent == nil {
					break Sent
				}
			case <-complete:
				active--
				if sent == nil && active == 0 {
					break Sent
				}
			}
		}
	}
}

func independentRunner(requestc <-chan *colly.Request, complete chan<- struct{}) {
	for req := range requestc {
		req.Do()
		complete <- struct{}{}
	}
}

func (q *Queue) loadRequest(c *colly.Collector) (*colly.Request, error) {
	switch q.storage.(type) {
	/*
		case *InMemoryQueueStorage:
			r, err := q.storage.(*InMemoryQueueStorage).GetRequestPointer()
			//todo need to figure out why this is causing a panic of "nil pointer dereference"
			return r, err
	*/
	default:
		buf, err := q.storage.GetRequest()
		if err != nil {
			return nil, err
		}
		copied := make([]byte, len(buf))
		copy(copied, buf)
		return c.UnmarshalRequest(copied)
	}

}

// Init implements Storage.Init() function
func (q *InMemoryQueueStorage) Init() error {
	q.lock = &sync.RWMutex{}
	return nil
}

// AddRequest implements Storage.AddRequest() function
// Request must be serializable
func (q *InMemoryQueueStorage) AddRequest(r []byte) error {
	sreq := &colly.SerializableRequest{}
	err := json.Unmarshal(r, sreq)
	if err != nil {
		return err
	}

	u, err := url.Parse(sreq.URL)
	if err != nil {
		return err
	}

	ctx := colly.NewContext()
	for k, v := range sreq.Ctx {
		ctx.Put(k, v)
	}

	req := &colly.Request{
		Method:  sreq.Method,
		URL:     u,
		Depth:   sreq.Depth,
		Body:    bytes.NewReader(sreq.Body),
		Ctx:     ctx,
		Headers: &sreq.Headers,
	}

	return q.AddRequestPointer(req)
}

// AddRequestPointer Adds a request to InMemoryQueueStorage via pointer to the request without JSON serialization
func (q *InMemoryQueueStorage) AddRequestPointer(r *colly.Request) error {
	q.lock.Lock()
	defer q.lock.Unlock()
	// Discard URLs if size limit exceeded
	if q.MaxSize > 0 && q.size >= q.MaxSize {
		return colly.ErrQueueFull
	}
	i := &inMemoryQueueItem{Request: r}
	if q.first == nil {
		q.first = i
	} else {
		q.last.Next = i
	}
	q.last = i
	q.size++
	return nil
}

// GetRequest implements Storage.GetRequest() function
// returns the serialized request as []byte and any error when encountered
func (q *InMemoryQueueStorage) GetRequest() ([]byte, error) {
	r, err := q.GetRequestPointer()
	if err != nil {
		return []byte{}, nil
	}
	return r.Marshal()
}

// GetRequestPointer request to InMemoryQueueStorage via pointer without without JSON serialization
// returns a pointer to a colly.Request and any error when encountered
func (q *InMemoryQueueStorage) GetRequestPointer() (*colly.Request, error) {
	q.lock.Lock()
	defer q.lock.Unlock()
	if q.size == 0 {
		return nil, nil
	}
	r := q.first.Request
	q.first = q.first.Next
	q.size--
	return r, nil
}

// QueueSize implements Storage.QueueSize() function
func (q *InMemoryQueueStorage) QueueSize() (int, error) {
	q.lock.Lock()
	defer q.lock.Unlock()
	return q.size, nil
}
