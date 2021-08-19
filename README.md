# filecache caches build artifacts in files
It is a copy of the Go internal cmd/go/internal/cache which is used for go build caching.

The default setup from the environment (GOCACHE) has been removed,
but the trimming defaults (1 hour mtime resolution, evict files older than 5 days)
has been kept. It is also possible to override these with TrimWithLimits.
