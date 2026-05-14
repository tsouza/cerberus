// Package logql is the from-scratch LogQL evaluator that serves as the
// independent specification for cerberus's property-testing framework.
// It implements the subset of LogQL log-stream semantics the property
// test exercises, in cerberus's tree, from the [LogQL semantics
// documentation] and Loki's documented filter / format behavior. The
// whole point is to catch bugs cerberus shares with upstream Loki:
// delegating to Loki's own engine would mask any bug both
// implementations share.
//
// # MVP coverage (Phase 1)
//
//   - Stream selectors: `{label="value"}` with the four matcher kinds
//     (=, !=, =~, !~). Resource-attribute matchers go against the
//     in-memory record's ResourceAttributes map — the same logical
//     column cerberus's lowering maps to ResourceAttributes[<label>].
//   - Line filters: `|=` / `!=` (substring) and `|~` / `!~` (regex)
//     applied to the record body. Multiple stages compose left-to-
//     right; chained `|=` are AND'd together (matches Loki's pipeline
//     semantics — see [LogQL pipeline]).
//   - `| label_format <new>=<src>` rename stages: copy `<src>` →
//     `<new>` in the per-record label set, drop `<src>` if `<new>`
//     differs. Matches Loki's documented rename semantics — see
//     [LogQL label_format].
//
// # Critical semantic decisions
//
// These are the points where the oracle MUST follow Loki semantics
// precisely:
//
//  1. Series identity: keyed by the post-pipeline ResourceAttributes
//     set. Two records with identical labels after `| label_format`
//     collapse into one stream (matches the cerberus handler's
//     [toStreamsWithTransform] grouping).
//  2. Line filter operands: `|=` / `!=` are case-sensitive substring
//     matches via Go's strings.Contains; `|~` / `!~` are full-match
//     RE2 regex via regexp.MatchString. This mirrors Loki's pipeline
//     log.NewFilter contract exactly — see [LogQL line filter].
//  3. `| label_format` rename idempotence: a rename whose source
//     label is missing on a record silently drops the new label too,
//     matching Loki's `lbs.GetWithCategory` early-return. The
//     property generator avoids this case (only renames pool labels
//     that every matched record actually carries), but the oracle
//     implements the silent-skip behaviour anyway so future
//     generator widenings don't need an oracle change.
//
// # Entry point
//
// Callers use [Evaluate], which is a pure function: it takes the
// dataset and a query (string form), parses the query via Loki's
// parser (so the AST shape matches what cerberus's pipeline sees),
// then walks the AST under in-tree evaluation rules. There is NO
// Loki engine call — the engine and its many extension points are
// exactly what we're independent of.
//
// # Output shape
//
// Each record that survives the pipeline contributes one
// property.OutcomeRow with:
//
//   - Labels    = ResourceAttributes (post-`| label_format`).
//   - TimestampMs = record nanoseconds / 1e6.
//   - Line      = the log line body.
//   - Value     = 0 (canonical for stream rows; the comparator
//     ignores Value when Line is non-empty).
//
// The property runner's comparator groups rows by Labels, then
// pairs each by (timestamp, line) — see [property.CompareOutcomes].
//
// [LogQL semantics documentation]: https://grafana.com/docs/loki/latest/query/log_queries/
// [LogQL pipeline]: https://grafana.com/docs/loki/latest/query/log_queries/#log-pipeline
// [LogQL line filter]: https://grafana.com/docs/loki/latest/query/log_queries/#line-filter-expression
// [LogQL label_format]: https://grafana.com/docs/loki/latest/query/log_queries/#labels-format-expression
package logql
