package chaudit

import (
	"context"
	"fmt"
)

// factsQuery collects, per metric in the window, the three factors the density
// guard multiplies. One pass over the window rather than a query per metric:
// an audit that costs as much as the panels it protects will not be run.
//
// Parameterised on the window only — the table is validated against the
// caller's own configured schema before it reaches here, and ClickHouse has no
// bind form for an identifier.
const factsQuery = `
SELECT
    MetricName,
    uniqExact(Attributes)                  AS series,
    count()                                AS raw_rows,
    max(length(ExplicitBounds))            AS bucket_width
FROM %s
WHERE TimeUnix > now() - toIntervalSecond(?)
GROUP BY MetricName
ORDER BY raw_rows * bucket_width * bucket_width DESC
LIMIT ?`

// amplifyQuery finds the label key whose removal collapses series cardinality
// the most, for ONE metric.
//
// The shape is the question "does this label carry meaning, or only identity":
// `uniqExact(Attributes)` against `uniqExact(mapFilter(k != key, Attributes))`.
// A label whose removal barely changes the count is describing something; a
// label whose removal collapses the count by orders of magnitude is minting
// identity per instance. The production case that motivated this (#2679) was
// 7,110 series over 98 route templates because `k8s_pod_name` sat beside
// `http_route` — a 72x multiplier buying nothing any panel displayed.
//
// arrayJoin over mapKeys enumerates the candidates, so a metric whose labels
// vary across series still has every key considered.
const amplifyQuery = `
SELECT
    key,
    uniqExact(Attributes)                                              AS total,
    uniqExact(mapFilter((k, v) -> k != key, Attributes))               AS without_key
FROM (
    SELECT Attributes, arrayJoin(mapKeys(Attributes)) AS key
    FROM %s
    WHERE TimeUnix > now() - toIntervalSecond(?) AND MetricName = ?
)
GROUP BY key
ORDER BY total / greatest(without_key, 1) DESC
LIMIT 1`

// minAmplificationRatio is the collapse factor below which a label is not
// worth naming. A label that merely doubles cardinality is usually carrying
// real meaning; one that multiplies it several-fold is the shape of an
// instance identifier that leaked into a metric's label set. Set low enough to
// catch a genuine defect early, high enough that an ordinary dimension
// (status code, method) is not reported as a problem.
const minAmplificationRatio = 4.0

// Probe runs the audit against a live ClickHouse.
//
// A failure to establish the amplifying label for one metric is recorded in
// Notes and does not abort the run: the budget standing is the load-bearing
// half of the report, and losing the remedy hint for one metric must not cost
// the operator the whole audit.
func Probe(ctx context.Context, q Querier, opts Options) (Report, error) {
	if err := opts.Validate(); err != nil {
		return Report{}, err
	}
	rep := Report{
		SchemaVersion: ReportVersion,
		Table:         opts.Table,
		WindowSeconds: opts.WindowSeconds,
		Anchors:       opts.Anchors,
	}

	rows, err := q.QueryContext(ctx, fmt.Sprintf(factsQuery, opts.Table), opts.WindowSeconds, opts.Top)
	if err != nil {
		return Report{}, fmt.Errorf("chaudit: probing %s: %w", opts.Table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var m MetricAudit
		if err := rows.Scan(&m.Metric, &m.Series, &m.RawRows, &m.BucketWidth); err != nil {
			return Report{}, fmt.Errorf("chaudit: scanning %s facts: %w", opts.Table, err)
		}
		m.Budget = opts.DensityUnitBudget
		m.CostUnits = costUnits(m.Series, opts.Anchors, m.RawRows, m.BucketWidth)
		m.HeadroomPct = headroomPct(m.CostUnits, m.Budget)
		rep.Metrics = append(rep.Metrics, m)
	}
	if err := rows.Err(); err != nil {
		return Report{}, fmt.Errorf("chaudit: reading %s facts: %w", opts.Table, err)
	}

	for i := range rep.Metrics {
		label, ratio, err := probeAmplifyingLabel(ctx, q, opts, rep.Metrics[i].Metric)
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"could not establish the amplifying label for %q (%v); its budget standing above is unaffected",
				rep.Metrics[i].Metric, err,
			))
			continue
		}
		if ratio >= minAmplificationRatio {
			rep.Metrics[i].AmplifyingLabel = label
			rep.Metrics[i].AmplificationRatio = ratio
		}
	}

	rankByHeadroom(rep.Metrics)
	return rep, nil
}

// probeAmplifyingLabel returns the worst offender for one metric, and the
// factor by which it multiplies series count.
func probeAmplifyingLabel(ctx context.Context, q Querier, opts Options, metric string) (string, float64, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf(amplifyQuery, opts.Table), opts.WindowSeconds, metric)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", 0, err
		}
		// A metric with no labels at all is legitimate and not a defect.
		return "", 0, nil
	}
	var key string
	var total, without int64
	if err := rows.Scan(&key, &total, &without); err != nil {
		return "", 0, err
	}
	// A label is an AMPLIFIER only if it multiplies a grouping that survives
	// without it. Removing the sole distinguishing label of a metric always
	// collapses it to one series, which scores maximally on the ratio alone —
	// so ratio by itself would accuse every well-shaped single-dimension
	// metric of the exact defect this audit exists to find. Requiring the
	// remainder to still carry structure is what separates "this label IS the
	// dimension" from "this label multiplies the dimension".
	if without <= 1 {
		return "", 0, nil
	}
	return key, float64(total) / float64(without), rows.Err()
}
