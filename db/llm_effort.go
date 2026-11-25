package db

import (
	"database/sql"
	"errors"
	"time"
)

// ErrAgentNotFound is returned when an override targets an agent key that does
// not exist. The API maps it to 404.
var ErrAgentNotFound = errors.New("agent not found")

// ReasoningEffortLevels lists every accepted reasoning-effort value. "" means
// "not configured": an override row is never stored with an empty value, and a
// profile keeps its own value when no override exists.
var ReasoningEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// ValidReasoningEffort reports whether v is one of ReasoningEffortLevels. The
// empty string is valid only for "no value" and is handled by callers, not here.
func ValidReasoningEffort(v string) bool {
	for _, level := range ReasoningEffortLevels {
		if v == level {
			return true
		}
	}
	return false
}

// AgentProfileEffort is one (agent, LLM profile) reasoning-effort override. It
// decouples "how hard this agent should think" from "which LLM config serves
// it", so the same agent can use a different effort per profile without cloning
// the whole profile.
type AgentProfileEffort struct {
	AgentKey        string    `json:"agent_key"`
	ProfileID       int64     `json:"profile_id"`
	ReasoningEffort string    `json:"reasoning_effort"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// AgentEffortRef is one agent's override on a profile, for the reverse list
// ("which agents override this profile").
type AgentEffortRef struct {
	AgentKey        string    `json:"agent_key"`
	ReasoningEffort string    `json:"reasoning_effort"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// AllEffortOverrides returns every override grouped by profile id. One query
// backs the profile page's reverse list, so it never does N+1 reads.
func (d *DB) AllEffortOverrides() (map[int64][]AgentEffortRef, error) {
	rows, err := d.Query(`SELECT profile_id, agent_key, reasoning_effort, updated_at
FROM agent_profile_effort_overrides ORDER BY profile_id, agent_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]AgentEffortRef{}
	for rows.Next() {
		var id int64
		var ref AgentEffortRef
		if err := rows.Scan(&id, &ref.AgentKey, &ref.ReasoningEffort, &ref.UpdatedAt); err != nil {
			return nil, err
		}
		out[id] = append(out[id], ref)
	}
	return out, rows.Err()
}

// AgentEffortRow is one LLM profile as seen from one agent's effort settings:
// the profile's own value, whether it has a key, and this agent's override.
type AgentEffortRow struct {
	ProfileID      int64
	Name           string
	Format         string
	Model          string
	ProfileEffort  string
	HasKey         bool
	IsDefault      bool
	OverrideEffort string
	UpdatedAt      *time.Time
}

// AgentEffortRows returns every LLM profile with this agent's override joined in,
// ordered like the failover pool (default first, then priority DESC, id ASC).
// One query keeps the Agent config page and the route view from doing N+1 reads.
func (d *DB) AgentEffortRows(agentKey string) ([]AgentEffortRow, error) {
	rows, err := d.Query(`
SELECT p.id, p.name, p.format, p.model, COALESCE(p.reasoning_effort,''),
       COALESCE(p.api_key,'') <> '', p.is_default,
       COALESCE(o.reasoning_effort,''), o.updated_at
FROM llm_profiles p
LEFT JOIN agent_profile_effort_overrides o ON o.profile_id=p.id AND o.agent_key=$1
ORDER BY p.is_default DESC, p.priority DESC, p.id ASC`, agentKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentEffortRow{}
	for rows.Next() {
		var r AgentEffortRow
		var updated sql.NullTime
		if err := rows.Scan(&r.ProfileID, &r.Name, &r.Format, &r.Model, &r.ProfileEffort,
			&r.HasKey, &r.IsDefault, &r.OverrideEffort, &updated); err != nil {
			return nil, err
		}
		if updated.Valid {
			ts := updated.Time
			r.UpdatedAt = &ts
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EffortOverrides returns every override for one agent, keyed by profile id.
// Used by the Agent config page and by per-request resolution.
func (d *DB) EffortOverrides(agentKey string) (map[int64]AgentProfileEffort, error) {
	rows, err := d.Query(`SELECT agent_key, profile_id, reasoning_effort, updated_at
FROM agent_profile_effort_overrides WHERE agent_key=$1`, agentKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]AgentProfileEffort{}
	for rows.Next() {
		var e AgentProfileEffort
		if err := rows.Scan(&e.AgentKey, &e.ProfileID, &e.ReasoningEffort, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out[e.ProfileID] = e
	}
	return out, rows.Err()
}

// EffortOverride reads one (agent, profile) override. ok=false covers both "no
// row" and "profile/agent gone", so callers fall back to the profile value.
func (d *DB) EffortOverride(agentKey string, profileID int64) (AgentProfileEffort, bool, error) {
	var e AgentProfileEffort
	err := d.QueryRow(`SELECT agent_key, profile_id, reasoning_effort, updated_at
FROM agent_profile_effort_overrides WHERE agent_key=$1 AND profile_id=$2`, agentKey, profileID).
		Scan(&e.AgentKey, &e.ProfileID, &e.ReasoningEffort, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return AgentProfileEffort{}, false, nil
	}
	if err != nil {
		return AgentProfileEffort{}, false, err
	}
	return e, true, nil
}

// SetEffortOverride inserts or replaces one override. Callers validate the value
// (ValidReasoningEffort); an empty value means "clear", which is
// DeleteEffortOverride's job, not a row with an empty effort.
//
// Lock order matches DeleteProfile (child rows → profile): the agent row first,
// then the profile with FOR KEY SHARE, so a concurrent profile deletion and this
// write serialize instead of racing the FK check. DeleteProfile also locks agents
// that reference a profile only through an override, so the two orders agree.
func (d *DB) SetEffortOverride(agentKey string, profileID int64, effort string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockAgentThenProfile(tx, agentKey, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO agent_profile_effort_overrides(agent_key, profile_id, reasoning_effort, updated_at)
VALUES ($1,$2,$3,now())
ON CONFLICT (agent_key, profile_id)
DO UPDATE SET reasoning_effort=EXCLUDED.reasoning_effort, updated_at=now()`, agentKey, profileID, effort); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteEffortOverride removes one override (falling back to the profile's own
// effort). found=false when there was nothing to delete — the API treats that as
// idempotent success but can report it. The agent row is locked first for the
// same reason as SetEffortOverride.
func (d *DB) DeleteEffortOverride(agentKey string, profileID int64) (bool, error) {
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var agentID int64
	err = tx.QueryRow(`SELECT id FROM agents WHERE key=$1 FOR UPDATE`, agentKey).Scan(&agentID)
	if err == sql.ErrNoRows {
		return false, ErrAgentNotFound
	}
	if err != nil {
		return false, err
	}
	res, err := tx.Exec(`DELETE FROM agent_profile_effort_overrides WHERE agent_key=$1 AND profile_id=$2`, agentKey, profileID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// lockAgentThenProfile is the shared child→parent lock order for override
// writes. FOR KEY SHARE on the profile conflicts with DeleteProfile's FOR UPDATE,
// which is exactly the serialization point we need.
func lockAgentThenProfile(tx *sql.Tx, agentKey string, profileID int64) error {
	var agentID int64
	err := tx.QueryRow(`SELECT id FROM agents WHERE key=$1 FOR UPDATE`, agentKey).Scan(&agentID)
	if err == sql.ErrNoRows {
		return ErrAgentNotFound
	}
	if err != nil {
		return err
	}
	var pid int64
	err = tx.QueryRow(`SELECT id FROM llm_profiles WHERE id=$1 FOR KEY SHARE`, profileID).Scan(&pid)
	if err == sql.ErrNoRows {
		return ErrLLMProfileNotFound
	}
	return err
}
