package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/birak/birak/internal/generation"
	"github.com/birak/birak/internal/quorum"
	"github.com/hashicorp/raft"
)

func (s *Service) Collect(ctx context.Context, id raft.ServerID, fence quorum.CollectionFence) (generation.Collection, error) {
	address, err := s.peer(id)
	if err != nil {
		return generation.Collection{}, err
	}
	b, _ := json.Marshal(fence)
	r, err := s.Transport.request(ctx, id, address, "POST", "/v1/collect", bytes.NewReader(b))
	if err != nil {
		return generation.Collection{}, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return generation.Collection{}, errors.New("peer collection refused")
	}
	var out generation.Collection
	err = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&out)
	return out, err
}
func (s *Service) collect(w http.ResponseWriter, r *http.Request, p Principal) {
	// Only the currently recognized leader may request deletion. Even that
	// identity cannot choose the live set or bypass the local committed fence.
	if s.Node().Status().Leader != raft.ServerID(p.ID) {
		http.Error(w, "leader required", 403)
		return
	}
	var fence quorum.CollectionFence
	if decodeBody(r, &fence) != nil {
		http.Error(w, "invalid fence", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.options.TransferTimeout)
	defer cancel()
	out, err := s.Node().LocalCollect(ctx, fence)
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	json.NewEncoder(w).Encode(out)
}
