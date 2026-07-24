package dal

// MaxBatchSize is the upper limit for batch operations like BatchDelete.
const MaxBatchSize = 500

// MaxObjectIDResults caps the number of object IDs returned by ID-listing
// helpers (FindIDsByContentTypePrefix, FindIDsByFilter, FindOwnerObjectIDPairs).
// Prevents OOM and Postgres parameter limit (65535) when used in subsequent
// IN (...) clauses. Callers needing more should paginate.
const MaxObjectIDResults = 10000
