package bump

import (
	"encoding/hex"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// hashFromDisplayHex parses a 64-char hex txid in DISPLAY order (the way
// WhatsOnChain and BSV explorers print txids) into a chainhash.Hash, which
// stores bytes in INTERNAL (wire) order. Bitcoin's display orientation is the
// byte-reverse of the wire orientation, so the bytes are reversed here. This
// mirrors hashFromHex in tools/fetch-block-fixture/merkle.go; it is duplicated
// rather than imported because that helper lives in package main.
func hashFromDisplayHex(displayHex string) (*chainhash.Hash, error) {
	if len(displayHex) != 64 {
		return nil, fmt.Errorf("expected 64 hex chars, got %d", len(displayHex))
	}
	b, err := hex.DecodeString(displayHex)
	if err != nil {
		return nil, fmt.Errorf("hex decode: %w", err)
	}
	rev := make([]byte, 32)
	for i := range b {
		rev[i] = b[31-i]
	}
	h, err := chainhash.NewHash(rev)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// BuildFullBlockBUMP constructs a compound BUMP that spans an ENTIRE block.
//
// It is the recovery path for blocks whose per-subtree STUMP/merkle data is no
// longer obtainable from any Teranode datahub: instead of merging sparse
// per-subtree STUMPs (BuildCompoundBUMP), it takes the block's complete ordered
// txid list — every leaf of the block merkle tree — and builds the full tree
// from scratch.
//
// The result is byte-identical in structure to a normally-built compound BUMP:
// level 0 holds one PathElement per txid, and every higher level is filled in
// by the SAME package helpers arcade uses for the live path
// (computeMissingHashes for the dense lower levels, padAndComputeBlockLevel for
// Bitcoin-canonical odd-level duplication). Because a fully-populated level 0
// has every sibling present, computeMissingHashes alone climbs the whole tree;
// padAndComputeBlockLevel is invoked from level 0 as a belt-and-braces pass so
// odd leaf counts get the canonical duplicate marker, exactly as the live
// builder would produce.
//
// Parameters:
//   - blockHeight:   the block's height; stored on the returned MerklePath.
//   - orderedTxids:  ALL txids of the block in merkle-leaf order (display hex,
//     as printed by explorers). Index 0 MUST be the coinbase.
//   - trackedTxids:  set of txids (display hex) that arcade tracks. A level-0
//     element is marked Txid=true iff its txid is in this set;
//     every other leaf is a plain hash (Txid=false / nil).
//
// The returned *transaction.MerklePath has a fully-populated tree; its
// ComputeRoot equals the block's canonical merkle root and can be checked with
// ValidateCompoundRoot before persisting.
func BuildFullBlockBUMP(blockHeight uint64, orderedTxids []string, trackedTxids map[string]bool) (*transaction.MerklePath, error) {
	if len(orderedTxids) == 0 {
		return nil, fmt.Errorf("no txids supplied for block at height %d", blockHeight)
	}
	if blockHeight > 0xFFFFFFFF {
		return nil, fmt.Errorf("block height %d exceeds uint32 range", blockHeight)
	}

	mp := &transaction.MerklePath{
		BlockHeight: uint32(blockHeight), //nolint:gosec // range checked above
	}

	// Level 0: one PathElement per block txid. Offset is the leaf index;
	// index 0 is the coinbase. Hashes are parsed from display hex into
	// chainhash's internal byte order via the package's existing helper so
	// the on-the-wire representation matches a live-built compound exactly.
	for i, txidHex := range orderedTxids {
		h, err := hashFromDisplayHex(txidHex)
		if err != nil {
			return nil, fmt.Errorf("parse txid at index %d (%q): %w", i, txidHex, err)
		}
		elem := &transaction.PathElement{
			Offset: uint64(i), //nolint:gosec // leaf index, non-negative and bounded by tx count
			Hash:   h,
		}
		// Mark tracked transactions so per-tx minimal paths extracted from
		// this compound (ExtractMinimalPathForTx) carry the BRC-74 txid flag.
		if trackedTxids[txidHex] {
			tracked := true
			elem.Txid = &tracked
		}
		addLeaf(mp, 0, elem)
	}

	// Higher levels: reuse arcade's own tree-computation helpers so the
	// assembled compound is structurally identical to a live-built one.
	//
	// computeMissingHashes climbs every level where both children are
	// present — with a complete level 0 that is the whole tree. It does not,
	// however, insert the canonical duplicate marker for an odd node count,
	// so a single padAndComputeBlockLevel pass starting at level 0 adds those
	// markers and computes the corresponding parents. The number of levels in
	// a full block tree is ceil(log2(n)); pre-size Path so both helpers have
	// the levels they need to write into.
	totalHeight := treeHeight(len(orderedTxids))
	for len(mp.Path) < totalHeight {
		mp.Path = append(mp.Path, nil)
	}
	computeMissingHashes(mp)
	padAndComputeBlockLevel(mp, 0, len(orderedTxids))

	return mp, nil
}

// treeHeight returns the number of levels in a Bitcoin merkle tree with
// numLeaves leaves: 1 for a single (coinbase-only) block, ceil(log2(n))
// otherwise. A height-h tree has level indices 0..h-1 where level h-1 holds
// the single root.
func treeHeight(numLeaves int) int {
	if numLeaves <= 1 {
		return 1
	}
	height := 0
	for size := 1; size < numLeaves; size <<= 1 {
		height++
	}
	return height
}
