package storage

// Reader is satisfied structurally by any type with a Read method.
type Reader interface {
	Read(p []byte) (int, error)
}

// Closer is satisfied structurally by any type with a Close method.
type Closer interface {
	Close() error
}

// ReadCloser is satisfied by anything that satisfies both Reader and Closer.
type ReadCloser interface {
	Reader
	Closer
}

// FileStore is a concrete type that implements Reader, Closer, and (transitively)
// ReadCloser — none of those use an explicit `implements` keyword, which is the
// whole point of the structural-implements processor.
type FileStore struct {
	path string
}

func (f *FileStore) Read(p []byte) (int, error) {
	return 0, nil
}

func (f *FileStore) Close() error {
	return nil
}

// MemoryStore implements Reader only — should not implement Closer.
type MemoryStore struct {
	data []byte
}

func (m *MemoryStore) Read(p []byte) (int, error) {
	return 0, nil
}
