package util

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ResponseProducerAdvisoryLockSQL is shared by every code path that can
// create/complete an agent response lineage and by the operator adoption
// gate.  The gate holds the session-level form while it snapshots the source
// catalog and recreates the publisher; producer transactions take the
// transaction-scoped form so an in-flight write completes before the gate
// acquires the lock and no new write can enter during the transition.
const ResponseProducerAdvisoryLockSQL = `
SELECT pg_advisory_xact_lock(
  hashtextextended('multica.strikeflow.response.producer.freeze', 0)
)`

// LockResponseProducer serializes a response-producing transaction with the
// receipt-lineage adoption gate.  Keep this helper deliberately tiny so the
// exact lock identity cannot drift between handlers and the deployment gate.
func LockResponseProducer(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, ResponseProducerAdvisoryLockSQL)
	return err
}
