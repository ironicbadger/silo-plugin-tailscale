package tailscale

import (
	"bytes"
	"context"
	"time"

	"tailscale.com/ipn"
)

// StateStore translates ipn's delete semantics to the host's byte-value store.
// Independent deadlines let tsnet flush state after its run context is canceled.
type StateStore struct{ Host Host }

var _ ipn.StateStore = StateStore{}

// The host cannot delete a key or distinguish nil from an empty byte slice.
// A versioned envelope preserves both states without exposing either to tsnet.
// Legacy nonempty values remain readable and are upgraded on their next write.
var stateEnvelope = []byte("\x00silo-tsnet-state")

const stateFormatVersion byte = 1

const (
	stateDeleted byte = iota
	statePresent
)

func (s StateStore) ReadState(key ipn.StateKey) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, found, err := s.Host.ReadInstanceState(ctx, string(key))
	if err != nil {
		return nil, &PublicError{"cannot read encrypted overlay state"}
	}
	if !found || len(b) == 0 {
		return nil, ipn.ErrStateNotExist
	}
	if !bytes.HasPrefix(b, stateEnvelope) {
		return b, nil
	}
	payload := b[len(stateEnvelope):]
	if len(payload) < 2 || payload[0] != stateFormatVersion {
		return nil, &PublicError{"invalid encrypted overlay state format"}
	}
	switch payload[1] {
	case stateDeleted:
		if len(payload) == 2 {
			return nil, ipn.ErrStateNotExist
		}
	case statePresent:
		return bytes.Clone(payload[2:]), nil
	}
	return nil, &PublicError{"invalid encrypted overlay state format"}
}

func (s StateStore) WriteState(key ipn.StateKey, value []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	envelope := make([]byte, len(stateEnvelope)+2, len(stateEnvelope)+2+len(value))
	copy(envelope, stateEnvelope)
	envelope[len(stateEnvelope)] = stateFormatVersion
	if value != nil {
		envelope[len(stateEnvelope)+1] = statePresent
		envelope = append(envelope, value...)
	}
	if err := s.Host.WriteInstanceState(ctx, string(key), envelope); err != nil {
		return &PublicError{"cannot save encrypted overlay state"}
	}
	return nil
}
