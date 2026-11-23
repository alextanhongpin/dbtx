package jobs

// SetAfterClaimHook runs f after a claim commits and before its result is
// used, so tests can interrupt the worker at that point.
func SetAfterClaimHook(w *Worker, f func()) { w.afterClaim = f }
