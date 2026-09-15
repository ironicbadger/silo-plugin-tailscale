package tailscale

import (
	"bytes"
	"errors"
	"testing"

	"tailscale.com/ipn"
)

func TestStoreDeletionAndEmptyValue(t *testing.T) {
	h := newHost()
	s := StateStore{h}
	if err := s.WriteState("key", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteState("key", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadState("key"); !errors.Is(err, ipn.ErrStateNotExist) {
		t.Fatalf("deleted state: %v", err)
	}
	if err := s.WriteState("key", []byte{}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadState("key"); err != nil || len(got) != 0 {
		t.Fatalf("empty state: %v, %v", got, err)
	}
	// Escaping the envelope itself must round-trip arbitrary byte values.
	want := append(bytes.Clone(stateEnvelope), stateDeleted)
	if err := s.WriteState("key", want); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadState("key"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("binary state failed to round trip: %v", err)
	}
}

func TestStoreReadsLegacyStateAndRejectsCorruptEnvelope(t *testing.T) {
	h := newHost()
	s := StateStore{h}
	for _, legacy := range [][]byte{[]byte(`{"node":"existing"}`), []byte("private:key"), []byte("-----BEGIN PRIVATE KEY-----")} {
		h.values["key"] = bytes.Clone(legacy)
		got, err := s.ReadState("key")
		if err != nil || !bytes.Equal(got, legacy) {
			t.Fatal("legacy identity lost", err)
		}
	}
	h.values["key"] = []byte{}
	if _, err := s.ReadState("key"); !errors.Is(err, ipn.ErrStateNotExist) {
		t.Fatal("legacy deletion was not recognized")
	}
	for _, suffix := range [][]byte{nil, {stateFormatVersion}, {stateFormatVersion, stateDeleted, 1}, {2, statePresent}, {stateFormatVersion, 99}} {
		h.values["key"] = append(bytes.Clone(stateEnvelope), suffix...)
		if _, err := s.ReadState("key"); err == nil || errors.Is(err, ipn.ErrStateNotExist) {
			t.Fatalf("corrupt state accepted: %v", err)
		}
	}
}
