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
	"sync"
	"unicode/utf8"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

const Format = "birak-quorum-v1"
const maxCommandBytes = 64 << 10

var (
	ErrMembership  = errors.New("membership changed; retry operation")
	ErrTransition  = errors.New("membership transition in progress")
	ErrOperationID = errors.New("operation ID was already used for another mutation")
	ErrDataQuorum  = errors.New("insufficient durable generation copies")
)

type Mutation struct {
	ID     string         `json:"id"`
	Key    string         `json:"key"`
	Ref    generation.Ref `json:"ref"`
	Delete bool           `json:"delete,omitempty"`
}

func (m Mutation) validate() error {
	if m.ID == "" || len(m.ID) > 128 || !utf8.ValidString(m.ID) || m.Key == "" || len(m.Key) > 4096 || !utf8.ValidString(m.Key) {
		return errors.New("invalid operation ID or logical key")
	}
	if m.Delete {
		if m.Ref != (generation.Ref{}) {
			return errors.New("delete has generation")
		}
		return nil
	}
	return m.Ref.Validate()
}

func (m Mutation) fingerprint() string {
	b, _ := json.Marshal(m)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type Entry struct {
	Ref     generation.Ref `json:"ref"`
	Deleted bool           `json:"deleted"`
	Index   uint64         `json:"index"`
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
type command struct {
	Format      string
	Cluster     string
	Kind        string
	ConfigIndex uint64
	Mutation    Mutation
	Copies      []raft.ServerID
	Change      *transition
}

// state is reconstructed from committed log entries and snapshots, never from
// filesystem mtimes. Generations and operation IDs are retained without GC.
type state struct {
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
	if c.Format != Format || c.Cluster != s.Cluster {
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
		if s.Transition != nil {
			return ErrTransition
		}
		if !c.Mutation.Delete {
			v := voters(s.Config)
			seen := map[raft.ServerID]bool{}
			for _, id := range c.Copies {
				if v[id] {
					seen[id] = true
				}
			}
			if len(v) == 0 || len(seen) < len(v)/2+1 {
				return ErrDataQuorum
			}
			if old, ok := s.Generations[c.Mutation.Ref.Hash]; ok && old != c.Mutation.Ref {
				return errors.New("inconsistent generation size")
			}
			s.Generations[c.Mutation.Ref.Hash] = c.Mutation.Ref
		}
		e := Entry{Ref: c.Mutation.Ref, Deleted: c.Mutation.Delete, Index: log.Index}
		s.Entries[c.Mutation.Key] = e
		s.Operations[c.Mutation.ID] = receipt{Fingerprint: c.Mutation.fingerprint(), Entry: e}
		return e
	case "freeze":
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
	if s.Format != Format || s.Cluster != f.state.Cluster || s.Entries == nil || s.Operations == nil || s.Generations == nil || s.ConfigIndex == 0 || s.ConfigIndex > s.Index || len(voters(s.Config)) == 0 {
		return errors.New("invalid consensus snapshot")
	}
	for hash, ref := range s.Generations {
		if err := ref.Validate(); err != nil || hash != ref.Hash {
			return errors.New("invalid snapshot generation")
		}
	}
	checkEntry := func(e Entry) error {
		if e.Index == 0 || e.Index > s.Index {
			return errors.New("invalid entry index")
		}
		if e.Deleted {
			if e.Ref != (generation.Ref{}) {
				return errors.New("invalid tombstone")
			}
			return nil
		}
		if ref, ok := s.Generations[e.Ref.Hash]; !ok || ref != e.Ref {
			return errors.New("snapshot lost generation reference")
		}
		return nil
	}
	for k, e := range s.Entries {
		if k == "" || len(k) > 4096 || !utf8.ValidString(k) {
			return errors.New("invalid snapshot key")
		}
		if err := checkEntry(e); err != nil {
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
		if err := checkEntry(op.Entry); err != nil {
			return err
		}
	}
	if s.Transition != nil && (s.Transition.ID == "" || (s.Transition.Kind != "promote" && s.Transition.Kind != "remove")) {
		return errors.New("invalid snapshot transition")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
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
