package scan

// RootReporter is an optional extension of Reporter: a Reporter that also
// implements it is told when each root is done, while later roots are still
// being walked.
type RootReporter interface {
	// RootDone reports that root i (an index into Run's args) is walked and
	// every new or changed file under it is committed as an instance, so it is
	// findable by name. Pruning follows once all roots are done, then
	// hashing. Not called for a root cut short by a cancel
	// or an error. Called from Run's goroutine.
	RootDone(i int)
}
