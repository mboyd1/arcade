package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// queryTrackedTxids returns the subset of orderedTxids that arcade tracks —
// i.e. that already have a row in the transactions table. The returned map is
// keyed by display-hex txid with value true, ready to hand to
// bump.BuildFullBlockBUMP as its trackedTxids set.
//
// It opens its own short-lived pgx pool: arcade's postgres.Store deliberately
// does not expose a generic query surface, and this tool needs two ad-hoc
// statements (this SELECT and the MINED UPDATE) that are not part of the
// store.Store interface.
func queryTrackedTxids(ctx context.Context, dsn string, orderedTxids []string) (map[string]bool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	const q = `SELECT txid FROM transactions WHERE txid = ANY($1)`
	rows, err := pool.Query(ctx, q, orderedTxids)
	if err != nil {
		return nil, fmt.Errorf("select tracked txids: %w", err)
	}
	defer rows.Close()

	tracked := make(map[string]bool)
	for rows.Next() {
		var txid string
		if err := rows.Scan(&txid); err != nil {
			return nil, fmt.Errorf("scan tracked txid: %w", err)
		}
		tracked[txid] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracked txids: %w", err)
	}
	return tracked, nil
}

// markTrackedMined flips every tracked transaction in the block to MINED,
// stamping block_hash, block_height and timestamp_at. Rows already MINED are
// left untouched (the status <> 'MINED' guard) so the operation is idempotent
// and re-running --commit reports 0 affected rows the second time.
//
// It returns the number of rows updated.
func markTrackedMined(ctx context.Context, dsn, blockHash string, blockHeight uint64, trackedSet map[string]bool) (int64, error) {
	if len(trackedSet) == 0 {
		return 0, nil
	}
	txids := make([]string, 0, len(trackedSet))
	for txid := range trackedSet {
		txids = append(txids, txid)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return 0, fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	const q = `
UPDATE transactions
SET status='MINED', block_hash=$1, block_height=$2, timestamp_at=now()
WHERE txid = ANY($3) AND status <> 'MINED'`
	tag, err := pool.Exec(ctx, q, blockHash, int64(blockHeight), txids) //nolint:gosec // block height fits in int64
	if err != nil {
		return 0, fmt.Errorf("update tracked txs to MINED: %w", err)
	}
	return tag.RowsAffected(), nil
}
