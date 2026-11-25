package db

import (
	"database/sql"
	"time"
)

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
func (d *DB) SetEffortOverride(agentKey string, profileID int64, effort string) error {
	_, err := d.Exec(`INSERT INTO agent_profile_effort_overrides(agent_key, profile_id, reasoning_effort, updated_at)
VALUES ($1,$2,$3,now())
ON CONFLICT (agent_key, profile_id)
DO UPDATE SET reasoning_effort=EXCLUDED.reasoning_effort, updated_at=now()`, agentKey, profileID, effort)
	return err
}

// DeleteEffortOverride removes one override (falling back to the profile's own
// effort). found=false when there was nothing to delete — the API treats that as
// idempotent success but can report it.
func (d *DB) DeleteEffortOverride(agentKey string, profileID int64) (bool, error) {
	res, err := d.Exec(`DELETE FROM agent_profile_effort_overrides WHERE agent_key=$1 AND profile_id=$2`, agentKey, profileID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
