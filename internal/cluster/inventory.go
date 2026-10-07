package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/birak/birak/internal/generation"
	"github.com/hashicorp/raft"
)

func (s *Service) Inventory(ctx context.Context, id raft.ServerID, refs []generation.Ref) (generation.Inventory, error) {
	if len(refs) > generation.InventoryBatch {
		return generation.Inventory{}, errors.New("inventory batch too large")
	}
	address, err := s.peer(id)
	if err != nil {
		return generation.Inventory{}, err
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return generation.Inventory{}, err
	}
	r, err := s.Transport.request(ctx, id, address, "POST", "/v1/inventory", bytes.NewReader(b))
	if err != nil {
		return generation.Inventory{}, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return generation.Inventory{}, errors.New("peer inventory refused")
	}
	var out generation.Inventory
	err = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&out)
	return out, err
}
func (s *Service) inventory(w http.ResponseWriter, r *http.Request) {
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		http.Error(w, "transfer capacity exceeded", 503)
		return
	}
	var refs []generation.Ref
	if err := decodeBody(r, &refs); err != nil || len(refs) > generation.InventoryBatch {
		http.Error(w, "invalid inventory", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.options.TransferTimeout)
	defer cancel()
	out, err := s.Node().LocalInventory(ctx, refs)
	if err != nil {
		http.Error(w, "inventory not certified", 503)
		return
	}
	json.NewEncoder(w).Encode(out)
}
