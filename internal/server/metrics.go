package server

// Prometheus exposition, written by hand so the daemon keeps its "single binary,
// zero dependencies" property. The point of this endpoint is that the numbers
// worth paging on are numbers, not fields inside a JSON blob somebody has to
// parse: a stalled peer, a backlog that never drains, a file nobody can repair,
// and entries this node decided to skip forever.

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/birak/birak/internal/fileops"
)

type skipCounter interface{ SkippedEntries() int64 }

var started = time.Now()

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	out := &exposition{declared: make(map[string]bool)}

	if maxVer, err := s.store.MaxVersion(); err == nil {
		out.gauge("birak_max_version", "Highest version assigned by this node.", nil, float64(maxVer))
	}
	if count, err := s.store.FileCount(); err == nil {
		out.gauge("birak_files", "Live files known to this node.", nil, float64(count))
	}
	if repairs, err := s.store.RepairQueueStats(); err == nil {
		out.gauge("birak_repairs_queued", "Entries waiting in the repair queue.", nil, float64(repairs.Total))
		out.gauge("birak_repairs_due", "Repair entries whose backoff has elapsed.", nil, float64(repairs.Due))
		out.gauge("birak_repair_oldest_seconds", "Age of the oldest queued repair.", nil, float64(repairs.OldestAgeMS)/1000)
	}

	if local, ok := s.stats.(localStatsProvider); ok {
		status := local.ScanStatus()
		storageOK := local.CheckStorage() == nil
		out.gauge("birak_storage_ok", "1 when the data volume matches this metadata database.", nil, boolValue(storageOK))
		out.gauge("birak_local_ready", "1 when this node can serve and accept writes.", nil, boolValue(storageOK && status.Ready))
		out.gauge("birak_quarantined_files", "Files whose bytes changed without a write timestamp.", nil, float64(status.Quarantined))
		if status.LastScanAgoMS >= 0 {
			out.gauge("birak_last_scan_seconds", "Age of the last complete checksum scan.", nil, float64(status.LastScanAgoMS)/1000)
		}
	}
	if counter, ok := s.stats.(skipCounter); ok {
		out.counter("birak_skipped_entries_total",
			"Peer entries dropped as unusable or excluded, which leave no repair row.",
			float64(counter.SkippedEntries()))
	}

	if s.stats != nil {
		for _, peer := range s.stats.PeerStats() {
			labels := map[string]string{"peer": peer.Peer}
			out.gauge("birak_peer_lag", "Versions this node has not consumed from a peer.", labels, float64(peer.Lag))
			out.gauge("birak_peer_healthy", "1 when a peer's stream is moving and its queue is empty.", labels, boolValue(peer.Healthy))
			out.gauge("birak_peer_consecutive_errors", "Consecutive failed polls of a peer.", labels, float64(peer.ConsecutiveErrs))
			out.gauge("birak_peer_pending_repairs", "Repair entries queued for a peer.", labels, float64(peer.Pending))
			out.gauge("birak_peer_last_success_seconds", "Time since the last successful poll of a peer.", labels, float64(peer.LastSuccessAgo)/1000)
			out.gauge("birak_peer_last_reconcile_seconds", "Time since the last manifest comparison with a peer.", labels, float64(peer.LastReconcileMS)/1000)
		}
	}
	// Write throughput on a volume is bounded by how long its one commit lock
	// is held. Without this an operator cannot tell a slow disk from a
	// serialized node, which are fixed in completely different places.
	lock := fileops.LockStats(s.syncDir)
	out.counter("birak_commit_lock_held_seconds_total",
		"Time commits have spent holding this volume's lock.", lock.Held.Seconds())
	out.counter("birak_commit_lock_acquisitions_total",
		"Commits that have taken this volume's lock.", float64(lock.Acquisitions))
	out.gauge("birak_commit_lock_worst_seconds",
		"Longest single hold of this volume's lock.", nil, lock.Worst.Seconds())

	out.gauge("birak_uptime_seconds", "Time since this process started serving.", nil, time.Since(started).Seconds())

	io.WriteString(w, out.body.String())
}

// exposition emits the text format, declaring each metric's HELP/TYPE once.
type exposition struct {
	body     strings.Builder
	declared map[string]bool
}

func (e *exposition) gauge(name, help string, labels map[string]string, value float64) {
	e.write(name, "gauge", help, labels, value)
}

func (e *exposition) counter(name, help string, value float64) {
	e.write(name, "counter", help, nil, value)
}

func (e *exposition) write(name, kind, help string, labels map[string]string, value float64) {
	if !e.declared[name] {
		e.declared[name] = true
		fmt.Fprintf(&e.body, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	fmt.Fprintf(&e.body, "%s%s %g\n", name, formatLabels(labels), value)
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for key, value := range labels {
		// The quotes are written here, not by %q: the value is already escaped
		// for the exposition format, and %q would escape the escapes.
		parts = append(parts, key+`="`+escapeLabel(value)+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabel applies the exposition format's rules. A peer URL is operator
// input, so it is escaped rather than assumed to be label-safe.
func escapeLabel(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
