package scan

import "sync"

// RootReporter is an optional extension of Reporter: a Reporter that also
// implements it is told when each root is done, while the other roots are
// still being hashed.
type RootReporter interface {
	// RootDone reports that root i (an index into Run's args) is discovered
	// and every file under it is hashed and committed (or failed). Pruning
	// runs after all roots are done. Called from Run's goroutines.
	RootDone(i int)
}

// rootTracker counts each root's files from discovery to commit and calls
// RootDone once a root has none left. A nil tracker does nothing.
type rootTracker struct {
	r RootReporter

	mu         sync.Mutex
	pending    []int            // files yielded and not yet hashed
	lastBatch  []int            // batch holding the root's last hashed file, -1 none
	discovered []bool           // the walk is past the root
	fired      []bool           // RootDone called
	byPath     map[string][]int // yielded path -> roots (nested roots yield a path twice)
	committed  int              // batches committed
}

func newRootTracker(r Reporter, roots int) *rootTracker {
	rr, ok := r.(RootReporter)
	if !ok {
		return nil
	}
	t := &rootTracker{
		r:          rr,
		pending:    make([]int, roots),
		lastBatch:  make([]int, roots),
		discovered: make([]bool, roots),
		fired:      make([]bool, roots),
		byPath:     map[string][]int{},
	}
	for i := range t.lastBatch {
		t.lastBatch[i] = -1
	}
	return t
}

// yielded counts path p as a file of root i to hash.
func (t *rootTracker) yielded(i int, p string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.pending[i]++
	t.byPath[p] = append(t.byPath[p], i)
	t.mu.Unlock()
}

// walked marks root i fully discovered.
func (t *rootTracker) walked(i int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.discovered[i] = true
	t.fire()
}

// hashed settles path p's hash result; batch is the batch it was added to,
// or -1 when it failed.
func (t *rootTracker) hashed(p string, batch int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	roots := t.byPath[p]
	if len(roots) == 0 {
		t.mu.Unlock()
		return
	}
	i := roots[0]
	if len(roots) == 1 {
		delete(t.byPath, p)
	} else {
		t.byPath[p] = roots[1:]
	}
	t.pending[i]--
	t.lastBatch[i] = max(t.lastBatch[i], batch)
	t.fire()
}

// commitDone records that the first n batches are committed.
func (t *rootTracker) commitDone(n int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.committed = n
	t.fire()
}

// fire calls RootDone for every root that just became done. The caller holds
// t.mu; fire releases it before calling out.
func (t *rootTracker) fire() {
	var done []int
	for i := range t.fired {
		if !t.fired[i] && t.discovered[i] && t.pending[i] == 0 && t.lastBatch[i] < t.committed {
			t.fired[i] = true
			done = append(done, i)
		}
	}
	t.mu.Unlock()
	for _, i := range done {
		t.r.RootDone(i)
	}
}
