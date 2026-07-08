package main

import (
	"syscall"
	"testing"
)

// TestShadowPhaseSet asserts every shadow-side StableAttr.Ino carries the
// shadow phase bit; backing-side stays clean. Property: a backing inode N and
// a shadow inode N must never produce the same StableAttr.Ino (ADR-0008
// invariant 1).
func TestShadowPhaseSet(t *testing.T) {
	st := &syscall.Stat_t{Ino: 159, Dev: 42}
	backing := idFromStat(42, st)
	shadow := idFromShadowStat(42, st)

	if backing.Ino == shadow.Ino {
		t.Fatalf("backing and shadow Ino collide: %#x == %#x", backing.Ino, shadow.Ino)
	}
	if shadow.Ino&shadowPhase == 0 {
		t.Errorf("shadow Ino %#x missing shadowPhase bit %#x", shadow.Ino, shadowPhase)
	}
	if backing.Ino&shadowPhase != 0 {
		t.Errorf("backing Ino %#x unexpectedly has shadowPhase bit", backing.Ino)
	}
}

// TestPhaseDisjointAcrossInodes is the property check: for any (inode, dev)
// pair, the shadow and backing Inos are different even when underlying values
// align in adversarial ways.
func TestPhaseDisjointAcrossInodes(t *testing.T) {
	cases := []struct {
		name        string
		backingDev  uint64
		shadowDev   uint64
		ino         uint64
	}{
		{"low inode, same dev", 42, 42, 159},
		{"low inode, distinct devs", 42, 43, 159},
		{"high inode, same dev", 42, 42, 0x7fff_ffff_ffff_ffff},
		{"zero inode", 42, 42, 0},
		{"max uint32 inode (typical ext4)", 42, 42, 0xffff_ffff},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			backingSt := &syscall.Stat_t{Ino: c.ino, Dev: c.backingDev}
			shadowSt := &syscall.Stat_t{Ino: c.ino, Dev: c.shadowDev}
			backing := idFromStat(c.backingDev, backingSt)
			shadow := idFromShadowStat(c.shadowDev, shadowSt)
			if backing.Ino == shadow.Ino {
				t.Errorf("collision for ino=%#x: backing=%#x shadow=%#x", c.ino, backing.Ino, shadow.Ino)
			}
		})
	}
}

// TestShadowPhaseIsHighBit pins the specific bit choice. If we ever change
// shadowPhase, this test forces an explicit decision about whether the new
// value still leaves the Ino space disjoint from naturally-occurring inode
// numbers (which fit in uint32 on most Linux FSes).
func TestShadowPhaseIsHighBit(t *testing.T) {
	const want = uint64(1) << 63
	if shadowPhase != want {
		t.Errorf("shadowPhase = %#x, want %#x", shadowPhase, want)
	}
}
