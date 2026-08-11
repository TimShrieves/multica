package util

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ResponseProducerAdvisoryLockSQL is shared by every code path that can
// create or complete an agent response lineage and by the operator adoption
// gate. Producers use the transaction-scoped form so no lineage can be
// created while an operator holds the matching session-level lock.
const ResponseProducerAdvisoryLockSQL = `
SELECT pg_advisory_xact_lock(
  hashtextextended('multica.strikeflow.response.producer.freeze', 0)
)`

// LockResponseProducer serializes a response-producing transaction with the
// receipt-lineage adoption gate.
func LockResponseProducer(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, ResponseProducerAdvisoryLockSQL)
	return err
}
