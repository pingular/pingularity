//go:build !race

package store

// raceEnabled reports whether the race detector is instrumenting this build.
// Wall-clock budgets are meaningless under it - every memory access is
// instrumented, so the same work takes tens of times longer - and a perf guard
// that fails for that reason teaches people to ignore it.
const raceEnabled = false
