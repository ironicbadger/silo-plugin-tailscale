package tailscale

import (
	"context"
	"time"

	"tailscale.com/ipn"
)

// StateStore translates the host's missing marker to ipn.ErrStateNotExist.
// Independent deadlines let tsnet flush state after its run context is canceled.
type StateStore struct{ Host Host }

var _ ipn.StateStore = StateStore{}

func (s StateStore) ReadState(key ipn.StateKey) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, found, err := s.Host.ReadInstanceState(ctx, string(key))
	if err != nil {
		return nil, &PublicError{"cannot read encrypted overlay state"}
	}
	if !found {
		return nil, ipn.ErrStateNotExist
	}
	return b, nil
}

func (s StateStore) WriteState(key ipn.StateKey, value []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Host.WriteInstanceState(ctx, string(key), value); err != nil {
		return &PublicError{"cannot save encrypted overlay state"}
	}
	return nil
}
