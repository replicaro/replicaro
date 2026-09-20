package appdata

// AtomicReplace activates a completed file without exposing a partial write.
func AtomicReplace(source, destination string) error { return atomicReplace(source, destination) }
