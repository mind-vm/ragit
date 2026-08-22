package extract

// SetBinaryForTest points an IsolatedExtractor at a specific executable
// instead of this process's own, so tests can simulate a host binary that
// was never wired as an extraction child.
func SetBinaryForTest(e *IsolatedExtractor, path string) { e.binary = path }
