package deployment

import "sync"

// Activity counts in-flight requests per deployment. It is in-memory process
// state: counts are lost on restart, which is safe because a restart also
// drops the requests being counted. It is concurrency-safe.
type Activity struct {
	mu     sync.Mutex
	active map[ID]int
}

// NewActivity returns an empty tracker.
func NewActivity() *Activity { return &Activity{active: make(map[ID]int)} }

// Begin registers a request and returns the function that ends it. The
// returned function is idempotent, so it is safe to defer and also call on an
// early path.
func (a *Activity) Begin(id ID) (end func()) {
	a.mu.Lock()
	a.active[id]++
	a.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.active[id] <= 1 {
				delete(a.active, id)
				return
			}
			a.active[id]--
		})
	}
}

// Active returns the number of in-flight requests for the deployment.
func (a *Activity) Active(id ID) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active[id]
}
