package incident

import (
	"context"
	"encoding/json"
	"fmt"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/duyquang47/llm-rca/internal/builder"
)

// ManualScanStore persists individual scan-group results (the
// "service" and "k8s" groups builder.ScanService/ScanK8s produce) so
// a manual investigation survives an orchestrator pod restart between
// an `rcactl scan` and a later `rcactl analyze`. INSERT-only: ALTER
// TABLE UPDATE is an async ClickHouse mutation with unbounded
// latency, wrong for a flow where analyze can follow scan by seconds.
// Re-scanning just inserts another row; Latest always reads back the
// newest one per scan_type.
type ManualScanStore struct {
	conn ch.Conn
}

func NewManualScanStore(conn ch.Conn) *ManualScanStore { return &ManualScanStore{conn: conn} }

func (s *ManualScanStore) EnsureSchema(ctx context.Context) error {
	return s.conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS manual_scans (
			incident_id String,
			scan_type   Enum8('service' = 1, 'k8s' = 2),
			scanned_at  DateTime64(3),
			data        String
		) ENGINE = MergeTree ORDER BY (incident_id, scan_type, scanned_at)`)
}

// Save appends one scan result. scanType must be "service" or "k8s".
func (s *ManualScanStore) Save(ctx context.Context, incidentID, scanType string, data []byte) error {
	err := s.conn.Exec(ctx, `
		INSERT INTO manual_scans (incident_id, scan_type, scanned_at, data)
		VALUES (?, ?, now64(3), ?)`, incidentID, scanType, string(data))
	if err != nil {
		return fmt.Errorf("save manual scan: %w", err)
	}
	return nil
}

// Latest returns the most recent service/k8s scan for incidentID —
// nil for whichever group has never been scanned, distinct from a
// scan that ran and returned empty results.
func (s *ManualScanStore) Latest(ctx context.Context, incidentID string) (svc *builder.ServiceScan, k8s *builder.K8sScan, err error) {
	rows, err := s.conn.Query(ctx, `
		SELECT scan_type, data FROM manual_scans
		WHERE incident_id = ?
		ORDER BY scanned_at DESC LIMIT 1 BY scan_type`, incidentID)
	if err != nil {
		return nil, nil, fmt.Errorf("query manual scans: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var scanType, data string
		if err := rows.Scan(&scanType, &data); err != nil {
			return nil, nil, fmt.Errorf("scan manual scan row: %w", err)
		}
		switch scanType {
		case "service":
			var s builder.ServiceScan
			if err := json.Unmarshal([]byte(data), &s); err != nil {
				return nil, nil, fmt.Errorf("unmarshal service scan: %w", err)
			}
			svc = &s
		case "k8s":
			var k builder.K8sScan
			if err := json.Unmarshal([]byte(data), &k); err != nil {
				return nil, nil, fmt.Errorf("unmarshal k8s scan: %w", err)
			}
			k8s = &k
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read manual scans: %w", err)
	}
	return svc, k8s, nil
}

// HasScan reports whether incidentID has at least one saved scan of
// scanType — used by the incident list/show handlers to show SRE
// progress without loading the full scan payload.
func (s *ManualScanStore) HasScan(ctx context.Context, incidentID, scanType string) (bool, error) {
	row := s.conn.QueryRow(ctx, `
		SELECT count() FROM manual_scans WHERE incident_id = ? AND scan_type = ?`, incidentID, scanType)
	var count uint64
	if err := row.Scan(&count); err != nil {
		return false, fmt.Errorf("count manual scans: %w", err)
	}
	return count > 0, nil
}
