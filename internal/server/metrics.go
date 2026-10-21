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
	"sort"
	"strings"
	"time"

	"github.com/birak/birak/internal/fileops"
	"github.com/birak/birak/internal/gateway"
	"github.com/birak/birak/internal/watcher"
)

type skipCounter interface{ SkippedEntries() int64 }

type resumeCounter interface{ ResumedDownloads() (count, bytes int64) }

var started = time.Now()

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	out := &exposition{}

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
	// A peer restart replays its whole stream; on a converged node this stays
	// flat. A steady climb means work is being queued that the index already
	// answers.
	out.counter("birak_repairs_enqueued_total", "Repair queue rows written, new or updated.", float64(s.store.RepairEnqueued()))

	if local, ok := s.stats.(localStatsProvider); ok {
		status := local.ScanStatus()
		storageOK := local.CheckStorage() == nil
		out.gauge("birak_storage_ok", "1 when the data volume matches this metadata database.", nil, boolValue(storageOK))
		out.gauge("birak_local_ready", "1 when this node can serve and accept writes.", nil, boolValue(storageOK && status.Ready))
		out.gauge("birak_quarantined_files", "Files whose bytes changed without a write timestamp.", nil, float64(status.Quarantined))
		out.gauge("birak_replica_initialized", "1 after initial admission; does not certify current remote freshness.", nil, boolValue(status.ReplicaInitialized))
		out.gauge("birak_scrub_bytes_per_second", "Configured logical byte budget for checksum verification.", nil, float64(status.ScrubBytesPerSecond))
		out.gauge("birak_directory_sync_best_effort", "1 when directory flushing is best effort on this platform.", nil, boolValue(status.DirectorySyncBestEffort))
		if status.LastScrubAgoMS >= 0 {
			out.gauge("birak_last_scrub_seconds", "Age of the last completed checksum verification cycle.", nil, float64(status.LastScrubAgoMS)/1000)
		}
		// Always present: before the first sweep completes the age counts from
		// process start, so an alert on a sweep that never finishes fires
		// instead of watching a series that does not exist yet.
		lastScan := time.Since(started).Seconds()
		if status.LastScanAgoMS >= 0 {
			lastScan = float64(status.LastScanAgoMS) / 1000
		}
		out.gauge("birak_last_scan_seconds", "Age of the last complete sweep; time since start before the first one.", nil, lastScan)
		// Always present, so an alert on a name collision has a series to
		// watch before the first one happens.
		for _, reason := range watcher.SkipReasons {
			labels := map[string]string{"reason": reason}
			out.gauge("birak_scan_skipped_files", "Files the last complete scan found but could not index, by reason.", labels, float64(status.Skipped[reason]))
			out.labeledCounter("birak_scan_skipped_total", "Files left unindexed since start, by reason, event batches included.", labels, float64(status.SkippedTotal[reason]))
		}
		out.gauge("birak_external_deletions_held", "Files missing from disk held back from being published as deletions; nonzero is a fault.", nil, float64(status.HeldExternalDeletions))
		out.counter("birak_read_errors_total", "I/O errors reading indexed files; each quarantined the file for repair.", float64(status.ReadErrors))
	}
	// A full disk keeps the node ready and fails every write, so free space
	// must be visible here and not only in a host agent somebody forgot.
	for volume, path := range map[string]string{"sync": s.syncDir, "meta": s.metaDir} {
		if path == "" {
			continue
		}
		if free, total, err := diskSpace(path); err == nil {
			labels := map[string]string{"volume": volume}
			out.gauge("birak_disk_free_bytes", "Bytes available to the daemon on a volume.", labels, float64(free))
			out.gauge("birak_disk_total_bytes", "Size of a volume.", labels, float64(total))
		}
	}
	for name, classes := range gateway.RequestCounts() {
		for class, count := range classes {
			out.labeledCounter("birak_gateway_requests_total", "Gateway requests by status class.",
				map[string]string{"gateway": name, "code": class}, float64(count))
		}
	}
	if counter, ok := s.stats.(skipCounter); ok {
		out.counter("birak_skipped_entries_total",
			"Peer entries dropped as unusable or excluded, which leave no repair row.",
			float64(counter.SkippedEntries()))
	}
	if counter, ok := s.stats.(resumeCounter); ok {
		count, bytes := counter.ResumedDownloads()
		out.counter("birak_downloads_resumed_total", "Replica downloads continued from a partial transfer.", float64(count))
		out.counter("birak_download_resumed_bytes_total", "Bytes a resumed download did not have to transfer again.", float64(bytes))
	}

	if _, ok := s.stats.(peerIdentities); ok {
		out.gauge("birak_unconfigured_pollers", "Nodes polling this one that it does not poll; their peer lists disagree.", nil, float64(len(s.unconfiguredPollers())))
	}
	if s.stats != nil {
		for _, peer := range s.stats.PeerStats() {
			labels := map[string]string{"peer": peer.Peer}
			out.gauge("birak_peer_lag", "Versions this node has not consumed from a peer.", labels, float64(peer.Lag))
			out.gauge("birak_peer_healthy", "1 when a peer's stream is moving and its queue is empty.", labels, boolValue(peer.Healthy))
			out.gauge("birak_peer_consecutive_errors", "Consecutive failed polls of a peer.", labels, float64(peer.ConsecutiveErrs))
			out.gauge("birak_peer_pending_repairs", "Repair entries queued for a peer.", labels, float64(peer.Pending))
			out.gauge("birak_peer_duplicate_node_id", "1 while a peer answers with this node's node_id; neither replicates from the other.", labels, boolValue(peer.DuplicateNodeID))
			// A peer that never answered is as old as this process, not fresh.
			lastSuccess := time.Since(started).Seconds()
			if peer.LastSuccessAgo >= 0 {
				lastSuccess = float64(peer.LastSuccessAgo) / 1000
			}
			out.gauge("birak_peer_last_success_seconds", "Time since the last successful poll of a peer; time since start if none.", labels, lastSuccess)
			if peer.LastReconcileMS >= 0 {
				out.gauge("birak_peer_last_reconcile_seconds", "Time since the last completed manifest comparison with a peer.", labels, float64(peer.LastReconcileMS)/1000)
			}
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
	reads := fileops.ReaderStats(s.syncDir)
	out.gauge("birak_active_readers", "Open revocable filesystem readers across gateways and peers.", nil, float64(reads.Active))
	out.counter("birak_file_read_bytes_total", "Bytes read from filesystem descriptors; includes probes and peer traffic, not confirmed egress.", float64(reads.BytesRead))
	out.counter("birak_reader_revocations_total", "Open readers revoked after integrity detection.", float64(reads.Revocations))

	out.gauge("birak_uptime_seconds", "Time since this process started serving.", nil, time.Since(started).Seconds())
	if s.version != "" {
		out.gauge("birak_build_info", "Daemon release; the value is always 1.", map[string]string{"version": s.version}, 1)
	}

	io.WriteString(w, out.String())
}

// exposition emits the text format. Samples are grouped per metric whatever
// order they were added in: the format requires one block per family, and
// per-peer and per-reason loops naturally interleave families.
type exposition struct {
	order    []string
	families map[string]*family
}

type family struct {
	header  string
	samples strings.Builder
}

func (e *exposition) String() string {
	var b strings.Builder
	for _, name := range e.order {
		f := e.families[name]
		b.WriteString(f.header)
		b.WriteString(f.samples.String())
	}
	return b.String()
}

func (e *exposition) gauge(name, help string, labels map[string]string, value float64) {
	e.write(name, "gauge", help, labels, value)
}

func (e *exposition) counter(name, help string, value float64) {
	e.write(name, "counter", help, nil, value)
}

func (e *exposition) labeledCounter(name, help string, labels map[string]string, value float64) {
	e.write(name, "counter", help, labels, value)
}

func (e *exposition) write(name, kind, help string, labels map[string]string, value float64) {
	f := e.families[name]
	if f == nil {
		if e.families == nil {
			e.families = make(map[string]*family)
		}
		f = &family{header: fmt.Sprintf("# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)}
		e.families[name] = f
		e.order = append(e.order, name)
	}
	fmt.Fprintf(&f.samples, "%s%s %g\n", name, formatLabels(labels), value)
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
	// Map order is random; a stable order keeps scrapes diffable.
	sort.Strings(parts)
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
