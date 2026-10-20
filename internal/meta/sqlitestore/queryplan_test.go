package sqlitestore

import (
	"strings"
	"testing"
)

// TestHotQueriesUseIndexes guards the queries whose cost grows with the
// library (measured with tools/perf, docs/PERFORMANCE.md): none of them may
// scan a large table. A scan of an index (SCAN ... USING [COVERING] INDEX)
// is fine where the query reads that index in order or a partial index.
func TestHotQueriesUseIndexes(t *testing.T) {
	s := open(t, newVolume(t))
	for name, q := range map[string]string{
		"snapshot page": "SELECT " + assetColumns + ` FROM folder_assets fa
			JOIN assets a ON a.asset_id = fa.asset_id JOIN blobs b ON b.sha256 = a.original_sha256
			WHERE fa.folder_id = 'f' AND fa.removed_at IS NULL AND fa.asset_id > '' ORDER BY fa.asset_id LIMIT 501`,
		"event page":             "SELECT seq, event_type, asset_id FROM folder_changes WHERE folder_id = 'f' AND seq > 0 ORDER BY seq LIMIT 501",
		"folders of an asset":    "SELECT fa.folder_id FROM assets a JOIN folder_assets fa ON fa.asset_id = a.asset_id WHERE a.original_sha256 = 'x'",
		"pending captures":       "SELECT sha256 FROM blobs WHERE capture_state = 'pending' UNION SELECT sha256 FROM blobs WHERE duration_state = 'pending' ORDER BY sha256 LIMIT 10",
		"pending previews":       "SELECT a.original_sha256 FROM assets a WHERE a.preview_state = 'pending'",
		"blob index (reconcile)": "SELECT sha256, bytes FROM blobs ORDER BY sha256",
		"receipt lookup":         "SELECT sha256 FROM upload_receipts WHERE user_id = 'u' AND device_id = 'd' AND folder_id = 'f' AND idempotency_key = 'k'",
		"receipt pruning":        "SELECT 1 FROM upload_receipts WHERE created_at < '2026'",
	} {
		rows, err := s.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		for _, d := range plan {
			if strings.HasPrefix(d, "SCAN ") && !strings.Contains(d, "INDEX") {
				t.Errorf("%s scans a table: %v", name, plan)
			}
			if strings.Contains(d, "USE TEMP B-TREE") {
				t.Errorf("%s sorts in a temporary b-tree: %v", name, plan)
			}
		}
	}
}
