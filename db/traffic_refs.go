package db

import (
	"context"
	"fmt"
)

// ProtectedTrafficIDs returns every traffic exchange ID that PostgreSQL still
// references: finding evidence snapshots, regression specs, finding-update
// timelines and exploration-node payloads. TTL retention must load this set
// before deleting anything and abort on error — an empty protection set due to
// a failed query is the one state where automatic cleanup would destroy proof.
//
// The query is deliberately broader than the current writers (jsonb path +
// explicit traffic_ids) so a future payload shape cannot silently lose
// protection.
func (d *DB) ProtectedTrafficIDs(ctx context.Context) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	rows, err := d.QueryContext(ctx, `
SELECT source_traffic_id FROM traffic_evidence_snapshots WHERE COALESCE(source_traffic_id,'') <> ''
UNION
SELECT baseline_traffic_id FROM finding_regressions WHERE COALESCE(baseline_traffic_id,'') <> ''
UNION
SELECT proof_traffic_id FROM finding_regressions WHERE COALESCE(proof_traffic_id,'') <> ''
UNION
SELECT v FROM finding_updates, LATERAL jsonb_array_elements_text(traffic_ids) AS v
WHERE jsonb_typeof(traffic_ids) = 'array'
UNION
SELECT v #>> '{}' FROM exploration_nodes, LATERAL jsonb_path_query(payload, '$.**.traffic_id') AS v
WHERE jsonb_typeof(v) = 'string'
UNION
SELECT v FROM exploration_nodes, LATERAL jsonb_array_elements_text(payload->'traffic_ids') AS v
WHERE jsonb_typeof(payload->'traffic_ids') = 'array'`)
	if err != nil {
		return nil, fmt.Errorf("读取流量保护集: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			out[id] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
