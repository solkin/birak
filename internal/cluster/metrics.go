package cluster

import (
	"fmt"
	"net/http"
)

// Operator-authenticated Prometheus text endpoint. Counters describe the latest
// local cycle, not an assertion that every replica is currently healthy.
func (s *Service) metrics(w http.ResponseWriter) {
	status := s.Node().Status()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	gauge := func(name string, value any) { fmt.Fprintf(w, "# TYPE %s gauge\n%s %v\n", name, name, value) }
	bit := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	gauge("birak_quorum_leader", bit(status.State == "Leader"))
	gauge("birak_quorum_voter", bit(status.Voter))
	gauge("birak_quorum_members", len(status.Members.Servers))
	gauge("birak_quorum_applied_index", status.Index)
	gauge("birak_quorum_collection_active", bit(status.Collection != nil))
	gauge("birak_quorum_backup_active", bit(status.BackupActive))
	gauge("birak_quorum_storage_error", bit(status.SpaceError != "" || status.StorageError != ""))
	gauge("birak_quorum_space_available_bytes", status.Space.Available)
	gauge("birak_quorum_space_reserved_bytes", status.Space.Reserved)
	gauge("birak_quorum_space_minimum_bytes", status.Space.Minimum)
	gauge("birak_quorum_scrub_running", bit(status.Maintenance.Running))
	gauge("birak_quorum_scrub_last_completed_seconds", float64(status.Maintenance.LastCompleted)/1e9)
	gauge("birak_quorum_scrub_failed", status.Maintenance.Failed)
	gauge("birak_quorum_scrub_repaired", status.Maintenance.Repaired)
	gauge("birak_quorum_repair_running", bit(status.Replication.Running))
	gauge("birak_quorum_repair_last_completed_seconds", float64(status.Replication.LastCompleted)/1e9)
	gauge("birak_quorum_repair_failed", status.Replication.Failed)
	gauge("birak_quorum_repair_copied", status.Replication.Repaired)
	gauge("birak_quorum_repair_pending", status.Replication.Pending)
}
