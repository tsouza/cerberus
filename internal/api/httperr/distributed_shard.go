package httperr

import (
	"errors"

	"github.com/tsouza/cerberus/internal/chclient"
)

// Client-facing texts for the two partial-shard-failure classes. Declared
// once here — every head (Prom, Loki, Tempo) renders the same fact to the
// same wire words, so the three handlers cannot drift apart on the
// explanation a dashboard operator reads. The leading "distributed shard
// unavailable:" names the class; the tail names which of the two
// ClickHouse rejections produced it.
const (
	shardUnavailableMessage      = "distributed shard unavailable: ClickHouse could not reach any replica of at least one data shard"
	staleReplicaFallbackMessage  = "distributed shard unavailable: every reachable replica of at least one data shard is stale; refusing to silently serve stale data"
	distributedShardClassMessage = "distributed shard unavailable"
)

// IsDistributedShardErr reports whether err is one of the ClickHouse
// Distributed-query error taxonomy's two partial-shard-failure classes
// (cerberus issue #3078): a data shard with no reachable replica at all
// (chclient.ErrShardUnavailable, CH code 279 ALL_CONNECTION_TRIES_FAILED,
// cerberus's own skip_unavailable_shards=0 pin) or one whose reachable
// replicas are all stale and cerberus's own
// fallback_to_stale_replicas_for_distributed_queries=0 pin refuses to
// silently serve one anyway (chclient.ErrStaleReplicaFallbackDenied, CH
// code 369 ALL_REPLICAS_ARE_STALE).
//
// Every head answers this class with HTTP 503 — the same "back off and
// retry" class as a tripped circuit breaker or a timed-out backend:
// ClickHouse itself is healthy, one data shard behind the Distributed
// table is not, and the client should neither treat it as a query-shape
// problem nor as a cerberus fault. The per-head envelope (errorType
// "unavailable" for Prom/Loki, ErrClassUnavailable for Tempo) stays with
// the head; the classification and the text are shared here.
func IsDistributedShardErr(err error) bool {
	return errors.Is(err, chclient.ErrShardUnavailable) || errors.Is(err, chclient.ErrStaleReplicaFallbackDenied)
}

// DistributedShardUnavailableMessage is the client-facing text for an err
// IsDistributedShardErr accepts: the text names which of the two rejections
// occurred. For any other err it falls back to the bare class name, so a
// caller that (wrongly) asks for the message of an unrelated error still
// gets a truthful, if unspecific, string rather than a misattribution.
func DistributedShardUnavailableMessage(err error) string {
	switch {
	case errors.Is(err, chclient.ErrShardUnavailable):
		return shardUnavailableMessage
	case errors.Is(err, chclient.ErrStaleReplicaFallbackDenied):
		return staleReplicaFallbackMessage
	}
	return distributedShardClassMessage
}
