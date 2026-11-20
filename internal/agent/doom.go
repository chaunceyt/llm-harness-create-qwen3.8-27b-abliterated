package agent

import (
	"harness/internal/llm"
	"harness/internal/tools"
)

// ringEntry is one recently executed call's correlation signature.
type ringEntry struct {
	sig     string
	isError bool
}

// doomState tracks repetitive and oscillating tool activity for one session.
// The ladder: trip once -> inject a reflection prompt; trip again while the
// pattern persists -> escalate to the human. A signature change defuses the
// ladder unless the ring still oscillates.
type doomState struct {
	last       string
	run        int
	reflected  bool
	ring       []ringEntry
	lastReason string
}

func newDoomState() *doomState { return &doomState{} }

// observe records one executed call's signature.
func (d *doomState) observe(call llm.ToolCall, res tools.Result) {
	sig := callDigest(call.Name, call.Arguments, res)
	if sig == d.last {
		d.run++
	} else {
		d.last = sig
		d.run = 1
	}
	d.ring = append(d.ring, ringEntry{sig: sig, isError: res.IsError})
	if len(d.ring) > 4 {
		d.ring = d.ring[len(d.ring)-4:]
	}
	if sig != d.last || d.run == 1 {
		// Signature just changed: defuse the ladder unless the ring still
		// oscillates.
		if !oscillates(d.ring) {
			d.reflected = false
		}
	}
}

// tripped reports whether the current pattern warrants the ladder.
func (d *doomState) tripped() bool {
	if d.run >= doomLimit {
		return true
	}
	return oscillates(d.ring)
}

// oscillates reports an alternating pair of distinct error signatures over
// the last four executions (ABAB).
func oscillates(ring []ringEntry) bool {
	if len(ring) != 4 {
		return false
	}
	r := ring
	return r[0].isError && r[1].isError && r[2].isError && r[3].isError &&
		r[0].sig == r[2].sig &&
		r[1].sig == r[3].sig &&
		r[0].sig != r[1].sig
}
