package quorum

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

const Format = "birak-quorum-v3"
const legacyFormat = "birak-quorum-v2"
const maxCommandBytes = 64 << 10

var (
	ErrMaintenance = errors.New("collection in progress or obsolete publication epoch")
	ErrMembership  = errors.New("membership changed; retry operation")
	ErrTransition  = errors.New("membership transition in progress")
	ErrOperationID = errors.New("operation ID was already used for another mutation")
	ErrDataQuorum  = errors.New("insufficient durable generation copies")
	ErrCondition   = errors.New("transaction precondition failed")
)

// Attributes are bounded metadata, not filesystem paths. Value holds small
// application records (buckets/uploads); file bytes always live in generations.
type Attributes struct {
	ETag        string `json:"etag,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Value       string `json:"value,omitempty"`
}

type Change struct {
	Key        string         `json:"key"`
	Ref        generation.Ref `json:"ref"`
	Delete     bool           `json:"delete,omitempty"`
	MetaOnly   bool           `json:"meta_only,omitempty"`
	Attributes Attributes     `json:"attributes,omitempty"`
}

// Index=0 requires absence (a tombstone counts as absent). EmptyPrefix guards
// bucket deletion against a concurrent object/upload creation in the FSM.
type Condition struct {
	Key         string `json:"key,omitempty"`
	Index       uint64 `json:"index,omitempty"`
	EmptyPrefix string `json:"empty_prefix,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	MaxCount    int    `json:"max_count,omitempty"`
}

type Mutation struct {
	NoReceipt  bool           `json:"no_receipt,omitempty"`
	ID         string         `json:"id"`
	Key        string         `json:"key"`
	Ref        generation.Ref `json:"ref"`
	Delete     bool           `json:"delete,omitempty"`
	Changes    []Change       `json:"changes,omitempty"`
	Conditions []Condition    `json:"conditions,omitempty"`
}

func (m Mutation) validate() error {
	if m.ID == "" || len(m.ID) > 128 || !utf8.ValidString(m.ID) {
		return errors.New("invalid operation ID")
	}
	if len(m.Changes) > 128 || len(m.Conditions) > 256 {
		return errors.New("transaction too large")
	}
	if len(m.Changes) > 0 && (m.Key != "" || m.Ref != (generation.Ref{}) || m.Delete) {
		return errors.New("ambiguous transaction")
	}
	seen := map[string]bool{}
	refs := map[string]generation.Ref{}
	for _, c := range m.changes() {
		if !validKey(c.Key) || seen[c.Key] {
			return errors.New("invalid or duplicate key")
		}
		seen[c.Key] = true
		if len(c.Attributes.ETag) > 128 || len(c.Attributes.ContentType) > 1024 || len(c.Attributes.Value) > 8192 {
			return errors.New("attributes too large")
		}
		if !utf8.ValidString(c.Attributes.ETag) || strings.ContainsAny(c.Attributes.ETag, "\r\n\x00") || !utf8.ValidString(c.Attributes.Value) || !utf8.ValidString(c.Attributes.ContentType) || strings.ContainsAny(c.Attributes.ContentType, "\r\n") {
			return errors.New("invalid attributes")
		}
		if c.Delete || c.MetaOnly {
			if c.Ref != (generation.Ref{}) || (c.Delete && c.MetaOnly) {
				return errors.New("invalid metadata change")
			}
		} else {
			if err := c.Ref.Validate(); err != nil {
				return err
			}
			if old, ok := refs[c.Ref.Hash]; ok && old != c.Ref {
				return errors.New("inconsistent transaction generation size")
			}
			refs[c.Ref.Hash] = c.Ref
		}
	}
	for _, c := range m.Conditions {
		if c.Prefix != "" {
			if !validKey(c.Prefix) || c.MaxCount <= 0 || c.Key != "" || c.EmptyPrefix != "" || c.Index != 0 {
				return errors.New("invalid count condition")
			}
		} else if c.MaxCount != 0 {
			return errors.New("count without prefix")
		} else if c.EmptyPrefix != "" {
			if c.Key != "" || c.Index != 0 || !validKey(c.EmptyPrefix) {
				return errors.New("invalid prefix condition")
			}
		} else if !validKey(c.Key) {
			return errors.New("invalid condition key")
		}
	}
	return nil
}

func validKey(key string) bool { return key != "" && len(key) <= 4096 && utf8.ValidString(key) }
func (m Mutation) changes() []Change {
	if len(m.Changes) > 0 {
		return m.Changes
	}
	return []Change{{Key: m.Key, Ref: m.Ref, Delete: m.Delete}}
}
func checkConditions(entries map[string]Entry, conditions []Condition) error {
	for _, c := range conditions {
		if c.Prefix != "" {
			count := 0
			for key, e := range entries {
				if !e.Deleted && strings.HasPrefix(key, c.Prefix) {
					count++
					if count >= c.MaxCount {
						return ErrCondition
					}
				}
			}
		} else if c.EmptyPrefix != "" {
			for key, e := range entries {
				if !e.Deleted && strings.HasPrefix(key, c.EmptyPrefix) {
					return ErrCondition
				}
			}
		} else {
			e, ok := entries[c.Key]
			if c.Index == 0 {
				if ok && !e.Deleted {
					return ErrCondition
				}
			} else if !ok || e.Deleted || e.Index != c.Index {
				return ErrCondition
			}
		}
	}
	return nil
}

func (m Mutation) fingerprint() string {
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Entry struct {
	Ref        generation.Ref `json:"ref"`
	Deleted    bool           `json:"deleted"`
	Index      uint64         `json:"index"`
	MetaOnly   bool           `json:"meta_only,omitempty"`
	Attributes Attributes     `json:"attributes,omitempty"`
	Modified   int64          `json:"modified,omitempty"`
}

type receipt struct {
	Fingerprint string
	Entry       Entry
}
type transition struct {
	Kind    string
	ID      raft.ServerID
	Address raft.ServerAddress
}
type CollectionFence struct {
	Epoch  uint64
	Before int64
}

type command struct {
	Epoch       uint64 `json:",omitempty"`
	Before      int64  `json:",omitempty"`
	Format      string
	Cluster     string
	Kind        string
	ConfigIndex uint64
	Mutation    Mutation
	Copies      []raft.ServerID
	Proofs      map[string][]raft.ServerID `json:",omitempty"`
	Timestamp   int64                      `json:",omitempty"`
	Change      *transition
}

// state is reconstructed from committed log entries and snapshots, never from
// filesystem mtimes. A persistent collection fence invalidates old byte proofs.
type state struct {
	Epoch       uint64
	Collection  *CollectionFence
	Format      string
	Cluster     string
	Index       uint64
	ConfigIndex uint64
	Config      raft.Configuration
	Entries     map[string]Entry
	Operations  map[string]receipt
	Generations map[string]generation.Ref
	Transition  *transition
}

type machine struct {
	mu    sync.RWMutex
	state state
}

func newMachine(cluster string) *machine {
	return &machine{state: state{Format: Format, Cluster: cluster, Entries: map[string]Entry{}, Operations: map[string]receipt{}, Generations: map[string]generation.Ref{}}}
}

func (f *machine) view() state {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s := f.state
	if s.Collection != nil {
		c := *s.Collection
		s.Collection = &c
	}
	s.Config = s.Config.Clone()
	s.Entries = maps.Clone(s.Entries)
	s.Operations = maps.Clone(s.Operations)
	s.Generations = maps.Clone(s.Generations)
	if s.Transition != nil {
		c := *s.Transition
		s.Transition = &c
	}
	return s
}

// control is O(number of members), independent of the number of stored files.
// Full map copies belong to snapshots/catch-up, never the ordinary write path.
func (f *machine) control() state {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s := f.state
	if s.Collection != nil {
		c := *s.Collection
		s.Collection = &c
	}
	s.Config = s.Config.Clone()
	s.Entries = nil
	s.Operations = nil
	s.Generations = nil
	if s.Transition != nil {
		c := *s.Transition
		s.Transition = &c
	}
	return s
}

func (f *machine) operation(id string) (receipt, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	op, ok := f.state.Operations[id]
	return op, ok
}

func (f *machine) entry(key string) (Entry, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.state.Entries[key]
	return e, ok
}

func (f *machine) catchupState() state {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s := f.state
	if s.Collection != nil {
		c := *s.Collection
		s.Collection = &c
	}
	s.Config = s.Config.Clone()
	s.Entries = nil
	s.Operations = nil
	s.Generations = maps.Clone(s.Generations)
	if s.Transition != nil {
		c := *s.Transition
		s.Transition = &c
	}
	return s
}

func voters(c raft.Configuration) map[raft.ServerID]bool {
	v := make(map[raft.ServerID]bool)
	for _, s := range c.Servers {
		if s.Suffrage == raft.Voter {
			v[s.ID] = true
		}
	}
	return v
}

func member(c raft.Configuration, id raft.ServerID) (raft.Server, bool) {
	for _, s := range c.Servers {
		if s.ID == id {
			return s, true
		}
	}
	return raft.Server{}, false
}

func (f *machine) StoreConfiguration(index uint64, config raft.Configuration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.ConfigIndex = index
	f.state.Index = index
	f.state.Config = config.Clone()
}

func (f *machine) Apply(log *raft.Log) interface{} {
	var c command
	if len(log.Data) > maxCommandBytes {
		return errors.New("oversized consensus command")
	}
	if err := json.Unmarshal(log.Data, &c); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &f.state
	s.Index = log.Index
	if (c.Format != Format && !(c.Format == legacyFormat && s.Epoch == 0)) || c.Cluster != s.Cluster {
		return errors.New("wrong consensus format or cluster")
	}
	if c.Kind == "mutate" {
		if err := c.Mutation.validate(); err != nil {
			return err
		}
		if old, ok := s.Operations[c.Mutation.ID]; ok {
			if old.Fingerprint != c.Mutation.fingerprint() {
				return ErrOperationID
			}
			return old.Entry
		}
	}
	if c.ConfigIndex == 0 || c.ConfigIndex != s.ConfigIndex {
		return ErrMembership
	}
	switch c.Kind {
	case "mutate":
		if s.Collection != nil || c.Epoch != s.Epoch {
			return ErrMaintenance
		}
		if s.Transition != nil {
			return ErrTransition
		}
		if err := checkConditions(s.Entries, c.Mutation.Conditions); err != nil {
			return err
		}
		changes := c.Mutation.changes()
		// Validate every output before changing any state: multipart completion
		// publishes the object and closes its upload in the same log entry.
		for _, change := range changes {
			if !change.Delete && !change.MetaOnly {
				v := voters(s.Config)
				seen := map[raft.ServerID]bool{}
				copies := c.Copies
				if len(c.Mutation.Changes) > 0 {
					copies = c.Proofs[change.Ref.Hash]
				}
				for _, id := range copies {
					if v[id] {
						seen[id] = true
					}
				}
				if len(v) == 0 || len(seen) < len(v)/2+1 {
					return ErrDataQuorum
				}
				if old, ok := s.Generations[change.Ref.Hash]; ok && old != change.Ref {
					return errors.New("inconsistent generation size")
				}
			}
		}
		var e Entry
		for i, change := range changes {
			out := Entry{Ref: change.Ref, Deleted: change.Delete, MetaOnly: change.MetaOnly, Attributes: change.Attributes, Index: log.Index, Modified: c.Timestamp}
			if !change.Delete && !change.MetaOnly {
				s.Generations[change.Ref.Hash] = change.Ref
			}
			s.Entries[change.Key] = out
			if i == 0 {
				e = out
			}
		}
		if !c.Mutation.NoReceipt {
			s.Operations[c.Mutation.ID] = receipt{Fingerprint: c.Mutation.fingerprint(), Entry: e}
		}
		return e
	case "collect-begin":
		if s.Transition != nil {
			return ErrTransition
		}
		if s.Collection != nil || c.Epoch != s.Epoch || c.Before <= 0 || s.Epoch == ^uint64(0) {
			return ErrMaintenance
		}
		s.Epoch++
		s.Collection = &CollectionFence{Epoch: s.Epoch, Before: c.Before}
		live := make(map[string]generation.Ref)
		for key, e := range s.Entries {
			if e.Deleted {
				if e.Modified < c.Before {
					delete(s.Entries, key)
				}
				continue
			}
			if !e.MetaOnly {
				live[e.Ref.Hash] = e.Ref
			}
		}
		s.Generations = live
		return nil
	case "collect-end":
		if c.Epoch != s.Epoch {
			return ErrMaintenance
		}
		s.Collection = nil
		return nil
	case "freeze":
		if s.Collection != nil {
			return ErrMaintenance
		}
		if c.Change == nil {
			return errors.New("missing membership transition")
		}
		if s.Transition != nil {
			if *s.Transition == *c.Change {
				return nil
			}
			return ErrTransition
		}
		m, ok := member(s.Config, c.Change.ID)
		if !ok || m.Address != c.Change.Address {
			return ErrMembership
		}
		switch c.Change.Kind {
		case "promote":
			if m.Suffrage != raft.Nonvoter {
				return ErrMembership
			}
		case "remove":
			if m.Suffrage == raft.Voter && len(voters(s.Config)) <= 1 {
				return errors.New("cannot remove last voter")
			}
		default:
			return errors.New("unknown membership transition")
		}
		copy := *c.Change
		s.Transition = &copy
		return nil
	case "thaw":
		if s.Transition == nil {
			return nil
		}
		if c.Change == nil || *c.Change != *s.Transition {
			return ErrTransition
		}
		m, exists := member(s.Config, c.Change.ID)
		if c.Change.Kind == "promote" && (!exists || m.Suffrage != raft.Voter) {
			return ErrMembership
		}
		if c.Change.Kind == "remove" && exists {
			return ErrMembership
		}
		s.Transition = nil
		return nil
	case "abort":
		if s.Transition == nil {
			return nil
		}
		if c.Change == nil || *c.Change != *s.Transition {
			return ErrTransition
		}
		m, exists := member(s.Config, c.Change.ID)
		// Cancellation is safe only before the membership operation commits.
		// The applied configuration index fences a concurrently pending change.
		if !exists || (c.Change.Kind == "promote" && m.Suffrage != raft.Nonvoter) {
			return ErrMembership
		}
		s.Transition = nil
		return nil
	default:
		return errors.New("unknown consensus command")
	}
}

type snapshot struct{ state state }

func (f *machine) Snapshot() (raft.FSMSnapshot, error) { return &snapshot{f.view()}, nil }
func (s *snapshot) Release()                           {}
func (s *snapshot) Persist(sink raft.SnapshotSink) error {
	if err := json.NewEncoder(sink).Encode(s.state); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (f *machine) Restore(r io.ReadCloser) error {
	defer r.Close()
	var s state
	d := json.NewDecoder(r)
	if err := d.Decode(&s); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("trailing snapshot data")
	}
	if (s.Format != Format && !(s.Format == legacyFormat && s.Epoch == 0 && s.Collection == nil)) || s.Cluster != f.state.Cluster || s.Entries == nil || s.Operations == nil || s.Generations == nil || s.ConfigIndex == 0 || s.ConfigIndex > s.Index || len(voters(s.Config)) == 0 {
		return errors.New("invalid consensus snapshot")
	}
	for hash, ref := range s.Generations {
		if err := ref.Validate(); err != nil || hash != ref.Hash {
			return errors.New("invalid snapshot generation")
		}
	}
	checkEntry := func(e Entry, live bool) error {
		if err := (Mutation{ID: "snapshot", Changes: []Change{{Key: "snapshot", Ref: e.Ref, Delete: e.Deleted, MetaOnly: e.MetaOnly, Attributes: e.Attributes}}}).validate(); err != nil {
			return err
		}
		if e.Index == 0 || e.Index > s.Index {
			return errors.New("invalid entry index")
		}
		if e.Deleted || e.MetaOnly {
			if e.Ref != (generation.Ref{}) {
				return errors.New("invalid tombstone")
			}
			return nil
		}
		if ref, ok := s.Generations[e.Ref.Hash]; live && (!ok || ref != e.Ref) {
			return errors.New("snapshot lost generation reference")
		}
		return nil
	}
	for k, e := range s.Entries {
		if k == "" || len(k) > 4096 || !utf8.ValidString(k) {
			return errors.New("invalid snapshot key")
		}
		if err := checkEntry(e, true); err != nil {
			return err
		}
	}
	for id, op := range s.Operations {
		if id == "" || len(id) > 128 {
			return errors.New("invalid snapshot operation ID")
		}
		b, err := hex.DecodeString(op.Fingerprint)
		if err != nil || len(b) != sha256.Size {
			return errors.New("invalid snapshot operation fingerprint")
		}
		if err := checkEntry(op.Entry, false); err != nil {
			return err
		}
	}
	if s.Transition != nil && (s.Transition.ID == "" || (s.Transition.Kind != "promote" && s.Transition.Kind != "remove")) {
		return errors.New("invalid snapshot transition")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.Collection != nil && (s.Collection.Epoch != s.Epoch || s.Epoch == 0 || s.Collection.Before <= 0 || s.Transition != nil) {
		return errors.New("invalid collection fence")
	}
	s.Format = Format
	f.state = s
	return nil
}

func encodeCommand(c command) ([]byte, error) {
	b, err := json.Marshal(c)
	if err == nil && len(b) > maxCommandBytes {
		err = fmt.Errorf("command exceeds %d bytes", maxCommandBytes)
	}
	return bytes.Clone(b), err
}
