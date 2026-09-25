package store

import (
	"bytes"
	"testing"
)

type mapKeys map[string]string

func (m mapKeys) Get(a string) (string, error) {
	v, ok := m[a]
	if !ok {
		return "", ErrNoEntry
	}
	return v, nil
}
func (m mapKeys) Set(a, v string) error { m[a] = v; return nil }
func (m mapKeys) Delete(a string) error { delete(m, a); return nil }

// The machine wrap binds a data key to its pack's id: the same sealed
// bytes presented for another pack do not open, and a flipped byte does
// not either. Behind Open the sealed tree's header also names its pack, so
// this binding is what keeps a key record honest on its own.
func TestTheMachineWrapBindsItsPack(t *testing.T) {
	s := &Store{opts: Options{Keys: mapKeys{}}}
	dk := bytes.Repeat([]byte{7}, 32)
	wk, err := s.machineWrap("kp_00000000000000000000000000000001", dk)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.machineUnwrap("kp_00000000000000000000000000000001", wk); err != nil || !bytes.Equal(got, dk) {
		t.Fatalf("its own pack: %v", err)
	}
	if _, err := s.machineUnwrap("kp_00000000000000000000000000000002", wk); err == nil {
		t.Fatal("a machine-wrapped key opened for another pack")
	}
	wk.Key[len(wk.Key)-1] ^= 1
	if _, err := s.machineUnwrap("kp_00000000000000000000000000000001", wk); err == nil {
		t.Fatal("an altered machine-wrapped key opened")
	}
}
