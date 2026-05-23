package bump

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// --- independent merkle oracle ---
//
// These mirror tools/fetch-block-fixture/merkle.go (parseHashes / merkleRoot)
// and serve as an independent check that BuildFullBlockBUMP produces the
// canonical Bitcoin merkle root. They are copied rather than imported because
// that reference lives in package main.

// oracleHashFromHex parses a display-hex txid into internal byte order.
func oracleHashFromHex(t *testing.T, displayHex string) chainhash.Hash {
	t.Helper()
	b, err := hex.DecodeString(displayHex)
	if err != nil || len(b) != 32 {
		t.Fatalf("oracleHashFromHex(%q): bad hex", displayHex)
	}
	rev := make([]byte, 32)
	for i := range b {
		rev[i] = b[31-i]
	}
	h, err := chainhash.NewHash(rev)
	if err != nil {
		t.Fatalf("oracleHashFromHex(%q): %v", displayHex, err)
	}
	return *h
}

// oracleMerkleRoot computes the Bitcoin merkle root of leaves, duplicating the
// last node on odd-count levels — the canonical convention.
func oracleMerkleRoot(leaves []chainhash.Hash) chainhash.Hash {
	if len(leaves) == 0 {
		return chainhash.Hash{}
	}
	current := leaves
	for len(current) > 1 {
		if len(current)%2 == 1 {
			current = append(current, current[len(current)-1])
		}
		next := make([]chainhash.Hash, 0, len(current)/2)
		for i := 0; i < len(current); i += 2 {
			parent := transaction.MerkleTreeParent(&current[i], &current[i+1])
			next = append(next, *parent)
		}
		current = next
	}
	return current[0]
}

// fakeTxidsDisplayHex returns n deterministic display-hex txids.
func fakeTxidsDisplayHex(n int) []string {
	out := make([]string, n)
	for i := range out {
		var b [32]byte
		// Spread the index across the bytes so the txids are visibly distinct
		// and none collide.
		b[0] = byte(i + 1)
		b[31] = byte(0xA0 + i)
		out[i] = hex.EncodeToString(b[:])
	}
	return out
}

func TestBuildFullBlockBUMP_RootMatchesOracle(t *testing.T) {
	// Cover both an even and an odd leaf count so the odd-level duplication
	// path in padAndComputeBlockLevel is exercised.
	for _, n := range []int{4, 5, 6, 8} {
		txids := fakeTxidsDisplayHex(n)

		compound, err := BuildFullBlockBUMP(950151, txids, nil)
		if err != nil {
			t.Fatalf("n=%d: BuildFullBlockBUMP: %v", n, err)
		}

		// Independent oracle root.
		leaves := make([]chainhash.Hash, n)
		for i, id := range txids {
			leaves[i] = oracleHashFromHex(t, id)
		}
		want := oracleMerkleRoot(leaves)

		// Root computed by the compound itself from a level-0 leaf.
		coinbase := oracleHashFromHex(t, txids[0])
		got, err := compound.ComputeRoot(&coinbase)
		if err != nil {
			t.Fatalf("n=%d: ComputeRoot: %v", n, err)
		}
		if !got.IsEqual(&want) {
			t.Fatalf("n=%d: compound root %s != oracle root %s", n, got, &want)
		}

		// ValidateCompoundRoot is the same check the CLI runs before persisting.
		if err := ValidateCompoundRoot(compound, &want); err != nil {
			t.Fatalf("n=%d: ValidateCompoundRoot: %v", n, err)
		}

		// Level 0 must hold one hash-bearing element per block txid, at
		// offsets 0..n-1. For odd n, padAndComputeBlockLevel additionally
		// appends a Bitcoin-canonical Duplicate marker at offset n — exactly
		// as a live-built compound BUMP would — so assert on hash elements.
		if len(compound.Path) == 0 {
			t.Fatalf("n=%d: compound has no levels", n)
		}
		for i := 0; i < n; i++ {
			elem := findLeafByOffset(compound, 0, uint64(i))
			if elem == nil || elem.Hash == nil {
				t.Fatalf("n=%d: missing level-0 hash at offset %d", n, i)
			}
			if !elem.Hash.IsEqual(&leaves[i]) {
				t.Fatalf("n=%d: level-0 offset %d hash mismatch", n, i)
			}
		}
	}
}

func TestBuildFullBlockBUMP_TrackedFlagAndExtractedPath(t *testing.T) {
	const n = 8
	txids := fakeTxidsDisplayHex(n)

	// Track a single non-coinbase tx.
	const trackedIdx = 5
	tracked := map[string]bool{txids[trackedIdx]: true}

	compound, err := BuildFullBlockBUMP(950151, txids, tracked)
	if err != nil {
		t.Fatalf("BuildFullBlockBUMP: %v", err)
	}

	// Exactly one level-0 element is flagged Txid=true, and it is the tracked one.
	trackedCount := 0
	for i, elem := range compound.Path[0] {
		isTxid := elem.Txid != nil && *elem.Txid
		if isTxid {
			trackedCount++
			if i != trackedIdx {
				t.Fatalf("Txid flag on offset %d, expected %d", i, trackedIdx)
			}
		}
	}
	if trackedCount != 1 {
		t.Fatalf("got %d Txid-flagged leaves, want 1", trackedCount)
	}

	// Extract a minimal path for the tracked tx and verify it computes the
	// canonical root via the go-sdk MerklePath API.
	trackedHash := oracleHashFromHex(t, txids[trackedIdx])
	minimal := ExtractMinimalPath(compound, uint64(trackedIdx))
	got, err := minimal.ComputeRoot(&trackedHash)
	if err != nil {
		t.Fatalf("minimal ComputeRoot: %v", err)
	}

	leaves := make([]chainhash.Hash, n)
	for i, id := range txids {
		leaves[i] = oracleHashFromHex(t, id)
	}
	want := oracleMerkleRoot(leaves)
	if !got.IsEqual(&want) {
		t.Fatalf("minimal-path root %s != oracle root %s", got, &want)
	}

	// Verify with a chaintracker that asserts the expected root — the same
	// path verification clients run.
	ok, err := minimal.Verify(context.Background(), &trackedHash, fixedRootTracker{root: &want})
	if err != nil {
		t.Fatalf("minimal Verify: %v", err)
	}
	if !ok {
		t.Fatalf("minimal path failed verification")
	}
}

func TestBuildFullBlockBUMP_Empty(t *testing.T) {
	if _, err := BuildFullBlockBUMP(950151, nil, nil); err == nil {
		t.Fatalf("expected error for empty txid list")
	}
}

func TestTreeHeight(t *testing.T) {
	cases := map[int]int{1: 1, 2: 1, 3: 2, 4: 2, 5: 3, 8: 3, 9: 4, 16: 4}
	for leaves, want := range cases {
		if got := treeHeight(leaves); got != want {
			t.Fatalf("treeHeight(%d) = %d, want %d", leaves, got, want)
		}
	}
}

// fixedRootTracker is a chaintracker.ChainTracker that recognises a single
// known merkle root at any height — enough for MerklePath.Verify in tests.
type fixedRootTracker struct {
	root *chainhash.Hash
}

func (f fixedRootTracker) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, _ uint32) (bool, error) {
	return root != nil && root.IsEqual(f.root), nil
}

func (f fixedRootTracker) CurrentHeight(_ context.Context) (uint32, error) {
	return 0, nil
}
