package db

// Read-only queries for the LLM route projection. They deliberately avoid every
// column that carries a secret: ProfileKeyPresence returns only booleans, and the
// route view uses ListProfiles (hint variant) rather than PoolProfiles, which
// loads api_key. Proxy/base_url may embed credentials, so callers must not echo
// them either.

// ProfileKeyPresence reports whether each LLM profile has an API key, without
// reading the key or its hint. Used by static route views to say "this profile
// cannot serve a run" honestly.
func (d *DB) ProfileKeyPresence() (map[int64]bool, error) {
	rows, err := d.Query(`SELECT id, COALESCE(api_key,'') <> '' FROM llm_profiles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		var has bool
		if err := rows.Scan(&id, &has); err != nil {
			return nil, err
		}
		out[id] = has
	}
	return out, rows.Err()
}

// TaskLLMContext exposes the one-statement task chain snapshot for read-only
// projections. The returned chain and cursor come from the same query, so they
// cannot describe two different revisions.
func (d *DB) TaskLLMContext(taskID int64) (legacy, active *int64, revision int64, chain []TaskLLMProfile, err error) {
	return d.taskLLMContext(taskID)
}
