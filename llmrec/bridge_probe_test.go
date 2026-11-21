package llmrec

import (
	"testing"

	"github.com/Autumn-27/artex/opencode"
)

// The recorder wraps every profile provider, so the bridge probe must see
// through it to the opencode provider underneath.
func TestBridgeBackedForwardsThroughRecorder(t *testing.T) {
	r := Wrap(opencode.NewProvider("m"), nil, "m", "p", "", "", nil)
	if !r.BridgeBacked() {
		t.Fatal("recorder over a bridge provider must report BridgeBacked")
	}
}
