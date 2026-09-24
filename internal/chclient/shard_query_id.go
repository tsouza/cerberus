package chclient

import (
	"strconv"
	"strings"
)

// A routed (route-B) request runs K shard statements, each a top-level
// ClickHouse query with its own query_id. The query_id of every one of them
// carries the request's identity and the statement's shard coordinates:
//
//	<request>-shard-<index>-of-<count>[-<redispatch>]
//
// <request> is one MintQueryID value shared by the request's K statements,
// <index> is the shard's position in [0, count), and <count> is K. A shard
// statement re-dispatched under a fresh id (the columnar decode's row-path
// fallback, freshQueryID) keeps the request and coordinates and appends a
// process-unique <redispatch> counter, since ClickHouse refuses two running
// queries with one id.
//
// The identity is what lets a reader of the query log that did not dispatch
// the request — another cerberus process, or this one after a restart — fold
// the K shard rows into the one observation the request is, rather than
// record K observations of a fraction of it each.
const (
	shardQueryIDInfix      = "-shard-"
	shardQueryIDCountInfix = "-of-"
	shardQueryIDSuffixSep  = "-"
)

// ShardQueryIDParts is a shard statement's identity, parsed from its query_id.
type ShardQueryIDParts struct {
	// Request is the id shared by every shard statement of one routed request.
	Request string
	// Index is this statement's shard, in [0, Count).
	Index int
	// Count is the request's shard count K.
	Count int
}

// ShardQueryID returns the query_id of shard index of a count-shard routed
// request whose own id is request (a MintQueryID value).
func ShardQueryID(request string, index, count int) string {
	return request + shardQueryIDInfix + strconv.Itoa(index) + shardQueryIDCountInfix + strconv.Itoa(count)
}

// ParseShardQueryID reports whether id is a shard statement's query_id and, if
// so, its identity. An id with no shard coordinates, or with coordinates that
// do not name a shard of the request (a non-numeric part, a count below one,
// an index outside [0, count)), is not a shard statement's.
func ParseShardQueryID(id string) (ShardQueryIDParts, bool) {
	at := strings.LastIndex(id, shardQueryIDInfix)
	if at <= 0 {
		return ShardQueryIDParts{}, false
	}
	indexText, rest, ok := strings.Cut(id[at+len(shardQueryIDInfix):], shardQueryIDCountInfix)
	if !ok {
		return ShardQueryIDParts{}, false
	}
	countText, _, _ := strings.Cut(rest, shardQueryIDSuffixSep)
	index, err := strconv.Atoi(indexText)
	if err != nil {
		return ShardQueryIDParts{}, false
	}
	count, err := strconv.Atoi(countText)
	if err != nil || count < 1 || index < 0 || index >= count {
		return ShardQueryIDParts{}, false
	}
	return ShardQueryIDParts{Request: id[:at], Index: index, Count: count}, true
}

// redispatchID returns a fresh query_id for a second physical execution of
// the same shard statement: the same request and coordinates, made unique by
// the process-global query-id counter.
func (p ShardQueryIDParts) redispatchID() string {
	return ShardQueryID(p.Request, p.Index, p.Count) + shardQueryIDSuffixSep + strconv.FormatUint(queryIDCounter.Add(1), 10)
}
