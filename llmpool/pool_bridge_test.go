package llmpool

import (
	"testing"

	"github.com/Autumn-27/artex/opencode"
)

// A chain containing one opencode-bridge member must report BridgeBacked so
// run-budget scaling sees through the pool; a chain of plain providers must not.
func TestPoolBridgeBacked(t *testing.T) {
	bridge := &Member{ID: 1, Name: "oc", Rank: 2, Prov: opencode.NewProvider("m")}
	native := &Member{ID: 2, Name: "api", Rank: 1, Prov: &fakeProv{}}

	if !(New([]*Member{bridge, native}, nil)).BridgeBacked() {
		t.Fatal("pool with a bridge member must report BridgeBacked")
	}
	if New([]*Member{native}, nil).BridgeBacked() {
		t.Fatal("pool without a bridge member must not report BridgeBacked")
	}
	if New(nil, nil) != nil {
		t.Fatal("empty chain should yield a nil pool")
	}
}
