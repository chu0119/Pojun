package db

import (
	"database/sql"
	"time"
)

// AgentPromptInfo is one (agent, LLM profile) independent-prompt override.
//
// Three states, by row presence and body:
//   - no row            → inherit the profile's default prompt
//   - row, empty text   → explicit "do not inject" for this agent
//   - row, text         → override the profile's default for this agent
//
// An override always applies, regardless of the profile's
// prompt_apply_inherited flag: that flag only gates the default layer.
type AgentPromptInfo struct {
	AgentKey  string    `json:"agent_key"`
	ProfileID int64     `json:"profile_id"`
	Text      string    `json:"prompt"`
	Mode      string    `json:"prompt_mode"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AgentPromptRef is one agent's override on a profile, for the profile page's
// reverse list. The body is represented by its character count only: the list
// must not ship potentially long prompts it never renders.
type AgentPromptRef struct {
	AgentKey    string    `json:"agent_key"`
	PromptMode  string    `json:"prompt_mode"`
	PromptChars int       `json:"prompt_chars"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AgentProfilePrompt reads one override. ok=false covers "no row" and
// "agent/profile gone"; both mean the caller inherits the profile default.
func (d *DB) AgentProfilePrompt(agentKey string, profileID int64) (AgentPromptInfo, bool, error) {
	var p AgentPromptInfo
	err := d.QueryRow(`SELECT agent_key, profile_id, prompt, prompt_mode, updated_at
FROM agent_profile_prompts WHERE agent_key=$1 AND profile_id=$2`, agentKey, profileID).
		Scan(&p.AgentKey, &p.ProfileID, &p.Text, &p.Mode, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return AgentPromptInfo{}, false, nil
	}
	if err != nil {
		return AgentPromptInfo{}, false, err
	}
	return p, true, nil
}

// AgentProfilePrompts returns every override for one agent, keyed by profile id.
func (d *DB) AgentProfilePrompts(agentKey string) (map[int64]AgentPromptInfo, error) {
	rows, err := d.Query(`SELECT agent_key, profile_id, prompt, prompt_mode, updated_at
FROM agent_profile_prompts WHERE agent_key=$1`, agentKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]AgentPromptInfo{}
	for rows.Next() {
		var p AgentPromptInfo
		if err := rows.Scan(&p.AgentKey, &p.ProfileID, &p.Text, &p.Mode, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out[p.ProfileID] = p
	}
	return out, rows.Err()
}

// AllAgentProfilePrompts returns every override grouped by profile id. One
// query backs the profile sheet's "which agents override this profile" list.
func (d *DB) AllAgentProfilePrompts() (map[int64][]AgentPromptRef, error) {
	rows, err := d.Query(`SELECT profile_id, agent_key, prompt_mode, length(prompt), updated_at
FROM agent_profile_prompts ORDER BY profile_id, agent_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]AgentPromptRef{}
	for rows.Next() {
		var id int64
		var ref AgentPromptRef
		if err := rows.Scan(&id, &ref.AgentKey, &ref.PromptMode, &ref.PromptChars, &ref.UpdatedAt); err != nil {
			return nil, err
		}
		out[id] = append(out[id], ref)
	}
	return out, rows.Err()
}

// SetAgentProfilePrompt inserts or replaces one override. An empty text stores
// an explicit "do not inject" row — different from deleting the row, which
// restores inheritance. Callers normalize text/mode and validate the enum; the
// database CHECK is only a backstop for direct writes.
//
// Lock order matches SetEffortOverride (child rows → profile): the agent row
// first, then the profile with FOR KEY SHARE, so a concurrent profile deletion
// and this write serialize instead of racing the FK check.
func (d *DB) SetAgentProfilePrompt(agentKey string, profileID int64, text, mode string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockAgentThenProfile(tx, agentKey, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO agent_profile_prompts(agent_key, profile_id, prompt, prompt_mode, updated_at)
VALUES ($1,$2,$3,$4,now())
ON CONFLICT (agent_key, profile_id)
DO UPDATE SET prompt=EXCLUDED.prompt, prompt_mode=EXCLUDED.prompt_mode, updated_at=now()`,
		agentKey, profileID, text, mode); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAgentProfilePrompt removes one override, restoring inheritance.
// found=false when there was nothing to delete — the API treats that as
// idempotent success. The agent row is locked first for the same reason as
// SetAgentProfilePrompt.
func (d *DB) DeleteAgentProfilePrompt(agentKey string, profileID int64) (bool, error) {
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
	res, err := tx.Exec(`DELETE FROM agent_profile_prompts WHERE agent_key=$1 AND profile_id=$2`, agentKey, profileID)
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
