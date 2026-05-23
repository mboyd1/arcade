// Command backfill-bump reconstructs and persists a compound BUMP merkle proof
// for a block whose per-subtree STUMP/merkle data is no longer available from
// any Teranode datahub.
//
// It rebuilds the proof from first principles: it fetches the block's complete
// ordered txid list from WhatsOnChain, builds the full block merkle tree via
// bump.BuildFullBlockBUMP, verifies the computed root against the canonical
// block-header merkle root, and — only with --commit — writes the compound
// BUMP into the bumps table and flips the block's tracked transactions to
// MINED.
//
// Usage (dry-run, no DB writes):
//
//	go run ./tools/backfill-bump \
//	    --block-hash 000000000000000000... \
//	    --block-height 950151 \
//	    --expected-merkleroot abcd... \
//	    --pg-dsn 'postgres://user:pass@host:5432/arcade'
//
// Add --commit to persist. Without --commit the tool performs NO writes and is
// safe to run repeatedly.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/bump"
	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/store/postgres"
)

const (
	wocBaseURL    = "https://api.whatsonchain.com/v1/bsv/main"
	wocPageSize   = 50000
	httpTimeout   = 60 * time.Second
	wocUserAgent  = "arcade-backfill-bump/1"
	maxBlockPages = 4096 // safety cap: 4096 * 50k leaves >> any real block
)

func main() {
	var (
		blockHash    = flag.String("block-hash", "", "block hash (display hex, required)")
		blockHeight  = flag.Uint64("block-height", 0, "block height (required)")
		expectedRoot = flag.String("expected-merkleroot", "", "canonical block-header merkle root, display hex (required)")
		pgDSN        = flag.String("pg-dsn", "", "Postgres DSN (required)")
		commit       = flag.Bool("commit", false, "persist the BUMP and mark txs MINED; default false = dry-run")
	)
	flag.Parse()

	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()

	if err := run(context.Background(), logger, *blockHash, *blockHeight, *expectedRoot, *pgDSN, *commit); err != nil {
		logger.Error("backfill failed", zap.Error(err))
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *zap.Logger, blockHash string, blockHeight uint64, expectedRoot, pgDSN string, commit bool) error {
	if blockHash == "" || blockHeight == 0 || expectedRoot == "" || pgDSN == "" {
		return fmt.Errorf("--block-hash, --block-height, --expected-merkleroot and --pg-dsn are all required")
	}

	expectedHash, err := chainhash.NewHashFromHex(expectedRoot)
	if err != nil {
		return fmt.Errorf("parse --expected-merkleroot %q: %w", expectedRoot, err)
	}

	// 1. Fetch the block's full ordered txid list from WhatsOnChain.
	logger.Info("fetching block txids from WhatsOnChain", zap.String("block_hash", blockHash))
	orderedTxids, headerMerkleRoot, err := fetchBlockTxids(ctx, logger, blockHash)
	if err != nil {
		return fmt.Errorf("fetch block txids: %w", err)
	}
	logger.Info("fetched block txids",
		zap.Int("txcount", len(orderedTxids)),
		zap.String("woc_merkleroot", headerMerkleRoot),
	)
	// Cross-check WhatsOnChain's own merkleroot against the caller-supplied
	// canonical root before we trust the txid list.
	if headerMerkleRoot != expectedRoot {
		return fmt.Errorf("WhatsOnChain merkleroot %s != --expected-merkleroot %s", headerMerkleRoot, expectedRoot)
	}

	// 2. Open the store and determine which txids arcade tracks.
	store, err := postgres.New(ctx, config.Postgres{DSN: pgDSN})
	if err != nil {
		return fmt.Errorf("open postgres store: %w", err)
	}
	defer func() { _ = store.Close() }()

	trackedSet, err := queryTrackedTxids(ctx, pgDSN, orderedTxids)
	if err != nil {
		return fmt.Errorf("query tracked txids: %w", err)
	}
	logger.Info("identified arcade-tracked txids", zap.Int("tracked_count", len(trackedSet)))

	// 3. Build the full-block compound BUMP.
	compound, err := bump.BuildFullBlockBUMP(blockHeight, orderedTxids, trackedSet)
	if err != nil {
		return fmt.Errorf("build full-block BUMP: %w", err)
	}

	// 4. Verify: the compound's computed root MUST equal the canonical root.
	// Fail hard before any DB write on mismatch.
	if err := bump.ValidateCompoundRoot(compound, expectedHash); err != nil {
		fmt.Printf("RESULT: FAIL\n")
		return fmt.Errorf("compound BUMP root verification failed: %w", err)
	}

	computedRoot, err := compound.ComputeRootHex(nil)
	if err != nil {
		return fmt.Errorf("compute root hex: %w", err)
	}

	// 5. Print the summary.
	fmt.Printf("block-hash:    %s\n", blockHash)
	fmt.Printf("block-height:  %d\n", blockHeight)
	fmt.Printf("txcount:       %d\n", len(orderedTxids))
	fmt.Printf("tracked:       %d\n", len(trackedSet))
	fmt.Printf("computed-root: %s\n", computedRoot)
	fmt.Printf("expected-root: %s\n", expectedRoot)
	fmt.Printf("bump-bytes:    %d\n", len(compound.Bytes()))
	fmt.Printf("RESULT:        OK\n")

	if !commit {
		fmt.Printf("dry-run: no database writes performed (pass --commit to persist)\n")
		return nil
	}

	// 6. Persist: store the compound BUMP and flip tracked txs to MINED.
	bumpBytes := compound.Bytes()
	if err := store.InsertBUMP(ctx, blockHash, blockHeight, bumpBytes); err != nil {
		return fmt.Errorf("insert BUMP: %w", err)
	}
	logger.Info("compound BUMP persisted", zap.String("block_hash", blockHash), zap.Int("bytes", len(bumpBytes)))

	mined, err := markTrackedMined(ctx, pgDSN, blockHash, blockHeight, trackedSet)
	if err != nil {
		return fmt.Errorf("mark tracked txs MINED: %w", err)
	}
	fmt.Printf("committed: BUMP stored, %d transaction rows set to MINED\n", mined)
	logger.Info("backfill committed", zap.Int64("rows_mined", mined))
	return nil
}

// --- WhatsOnChain block fetch ---

// wocBlock is the subset of the WhatsOnChain block-by-hash response this tool
// consumes.
type wocBlock struct {
	Hash       string   `json:"hash"`
	MerkleRoot string   `json:"merkleroot"`
	TxCount    int      `json:"txcount"`
	Tx         []string `json:"tx"`
	Pages      struct {
		URI []string `json:"uri"`
	} `json:"pages"`
}

// fetchBlockTxids retrieves the complete ordered txid list for a block.
// WhatsOnChain inlines the first 100 txids in the block response and exposes
// the remainder via paged endpoints (50000 txids per page). The assembled list
// is asserted to match the response's txcount.
func fetchBlockTxids(ctx context.Context, logger *zap.Logger, blockHash string) ([]string, string, error) {
	var blk wocBlock
	if err := getJSON(ctx, fmt.Sprintf("%s/block/hash/%s", wocBaseURL, blockHash), &blk); err != nil {
		return nil, "", fmt.Errorf("fetch block header: %w", err)
	}
	if blk.TxCount == 0 {
		return nil, "", fmt.Errorf("block %s reports txcount 0", blockHash)
	}

	txids := make([]string, 0, blk.TxCount)
	txids = append(txids, blk.Tx...)

	// When the block has more txids than the inline batch, walk the pages.
	for pageNum := 1; len(txids) < blk.TxCount; pageNum++ {
		if pageNum > maxBlockPages {
			return nil, "", fmt.Errorf("exceeded page cap %d fetching block %s", maxBlockPages, blockHash)
		}
		var page []string
		url := fmt.Sprintf("%s/block/hash/%s/page/%d", wocBaseURL, blockHash, pageNum)
		if err := getJSON(ctx, url, &page); err != nil {
			return nil, "", fmt.Errorf("fetch txid page %d: %w", pageNum, err)
		}
		if len(page) == 0 {
			return nil, "", fmt.Errorf("txid page %d for block %s is empty before reaching txcount %d", pageNum, blockHash, blk.TxCount)
		}
		logger.Info("fetched txid page", zap.Int("page", pageNum), zap.Int("page_size", len(page)))
		txids = append(txids, page...)
		// Pace requests so WhatsOnChain's rate limiter is not tripped; the
		// retry/backoff in getJSON is the safety net, this avoids needing it.
		if len(txids) < blk.TxCount {
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}

	if len(txids) != blk.TxCount {
		return nil, "", fmt.Errorf("assembled %d txids, expected txcount %d", len(txids), blk.TxCount)
	}
	return txids, blk.MerkleRoot, nil
}

// getJSON performs a GET and decodes the JSON body into out. WhatsOnChain
// rate-limits aggressively (HTTP 429) when block pages are fetched back to
// back, so transient 429/5xx responses are retried with exponential backoff.
func getJSON(ctx context.Context, url string, out any) error {
	const maxAttempts = 7
	backoff := 2 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		retryable, err := getJSONOnce(ctx, url, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || attempt == maxAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return lastErr
}

// getJSONOnce performs a single GET attempt. The bool return is true when the
// failure is transient (429 / 5xx) and worth retrying.
func getJSONOnce(ctx context.Context, url string, out any) (retryable bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", wocUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return true, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return transient, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, fmt.Errorf("decode %s: %w", url, err)
	}
	return false, nil
}

// _ keeps wocPageSize referenced for documentation purposes; WhatsOnChain's
// page size is fixed at 50000 and the loop above is page-count driven.
var _ = wocPageSize
