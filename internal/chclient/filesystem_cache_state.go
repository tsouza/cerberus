package chclient

import (
	"context"
	"fmt"
)

// filesystemCacheStateSQL reads the SERVER-CONFIGURED filesystem cache state
// directly from system.filesystem_cache_settings (cerberus issue #2780): one
// row per named cache an operator declared in the ClickHouse server config
// (storage_configuration's `cache` disk type wrapping the S3 disk). Summing
// across every configured cache — rather than reading a single named one —
// means the query needs no cache-name parameter and degrades cleanly to the
// all-zero row when no cache is configured at all (an aggregate over zero
// input rows still returns exactly one row: count()=0, every sum()=0, never
// NULL), so QueryFilesystemCacheState never needs nullable-column handling.
//
// This is DELIBERATELY not system.metrics' FilesystemCacheSize /
// FilesystemCacheElements gauges: those report bytes/elements resident
// across ALL caches process-wide with no per-cache breakdown and no
// configured-max context, so they cannot answer "is a cache even
// configured" — a zero reading is ambiguous between "no cache configured"
// and "a configured cache that is currently empty". system.filesystem_cache_settings's
// current_size/current_elements_num give the identical live counters WITH
// that context, in the same query as the configured max, so this is the
// single source of truth for the /info surface (see internal/api/info).
const filesystemCacheStateSQL = `SELECT count(), sum(max_size), sum(max_elements), sum(current_size), sum(current_elements_num) FROM system.filesystem_cache_settings`

// FilesystemCacheState is the live server-side filesystem cache reading
// (cerberus issue #2780): whether any named cache is configured, its
// aggregate configured capacity, and its aggregate current occupancy.
// Configured is the headline field — operator guidance
// (docs/operations.md's "Local filesystem cache" section) walks through
// adding a `cache` disk to the S3 storage_configuration; until that is done
// this reads Configured=false regardless of how many cerberus queries ran,
// because `enable_filesystem_cache` (the per-query toggle, already 1 by
// default on every ClickHouse version cerberus supports — verified live via
// chDB, see docs/clickhouse-optimizations.md's audit note) has nothing to
// cache into without a server-side cache disk.
type FilesystemCacheState struct {
	// Configured reports whether at least one named filesystem cache exists
	// on the connected server (system.filesystem_cache_settings returned a
	// non-zero row count).
	Configured bool
	// Caches is the number of named filesystem caches configured.
	Caches uint64
	// MaxSizeBytes is the summed configured max_size across every
	// configured cache.
	MaxSizeBytes uint64
	// MaxElements is the summed configured max_elements across every
	// configured cache.
	MaxElements uint64
	// CurrentSizeBytes is the summed current_size (live occupied bytes)
	// across every configured cache.
	CurrentSizeBytes uint64
	// CurrentElements is the summed current_elements_num (live occupied
	// file segments) across every configured cache.
	CurrentElements uint64
}

// QueryFilesystemCacheState runs filesystemCacheStateSQL and decodes the
// single aggregate row. Unlike the boot capability canaries (ProbeTSGridCapability,
// ProbeResultCacheCapability) this is not a one-shot boot probe: internal/api/info
// calls it on every GET /info request (under the handler's own bounded ping
// timeout) because cache occupancy is live state an operator watches change
// over the process lifetime, the same reason OptState's ServerVersion is a
// live closure rather than a boot-captured Snapshot field.
//
// Guarded by the circuit breaker (see [Client] doc): a struggling ClickHouse
// server does not get an extra query per /info poll on top of the readiness
// ping.
func (c *Client) QueryFilesystemCacheState(ctx context.Context) (FilesystemCacheState, error) {
	if !c.br.allow() {
		return FilesystemCacheState{}, c.br.openErr("chclient: query")
	}
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, filesystemCacheStateSQL, c.addr)
	defer span.End()
	defer flushProgress(ctx)
	rows, err := c.queryOpen(ctx, filesystemCacheStateSQL)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		return FilesystemCacheState{}, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	defer func() {
		_ = rows.Close()
	}()

	var out FilesystemCacheState
	if rows.Next() {
		if err := rows.Scan(&out.Caches, &out.MaxSizeBytes, &out.MaxElements, &out.CurrentSizeBytes, &out.CurrentElements); err != nil {
			return FilesystemCacheState{}, fmt.Errorf("chclient: scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return FilesystemCacheState{}, fmt.Errorf("chclient: rows.Err: %w", c.classifyDriverErr(ctx, err))
	}
	out.Configured = out.Caches > 0
	return out, nil
}
