//go:build chdb

package chsql_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// serverTimezones are the implicit ClickHouse timezones every time literal
// is evaluated under. Each query carries its zone as `session_timezone`,
// which overrides the server's implicit timezone for that query exactly as
// a server-level `timezone` would — so the pin does not depend on the host
// timezone chDB inherits. Pacific/Chatham (UTC+12:45 / +13:45) gives a
// non-whole-hour offset; America/Los_Angeles a negative one with DST.
var serverTimezones = []string{"UTC", "Pacific/Chatham", "America/Los_Angeles", "Asia/Kolkata"}

// literalInstants span a DST transition in both hemispheres' zones above and
// carry a non-zero nanosecond component so precision loss would surface too.
var literalInstants = []time.Time{
	time.Date(2026, 5, 14, 12, 0, 0, 123456789, time.UTC),
	time.Date(2026, 3, 8, 10, 30, 0, 1, time.UTC),
	time.Date(2026, 4, 5, 13, 59, 59, 999999999, time.UTC),
	time.Date(2023, 1, 15, 16, 8, 9, 0, time.FixedZone("UTC+5", 5*60*60)),
}

// TestTimeLiterals_DenoteTheSameInstantInEveryServerTimezone pins that both
// time-literal constructors — chplan.TimeLiteral (every plan-side bound and
// evaluation anchor, across all three heads) and chsql.Builder.DateTime64Lit
// (every emitter-side bound: range-window grids, metadata / search-tag /
// index-stats windows) — evaluate to the instant they were built from on a
// ClickHouse whose implicit timezone is not UTC.
func TestTimeLiterals_DenoteTheSameInstantInEveryServerTimezone(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)

	constructors := map[string]func(time.Time) (string, []any, error){
		"chplan.TimeLiteral": func(ts time.Time) (string, []any, error) {
			b := chsql.NewBuilder()
			if err := b.Expr(chplan.TimeLiteral(ts)); err != nil {
				return "", nil, err
			}
			sql, args, _ := b.Build()
			return sql, args, nil
		},
		"Builder.DateTime64Lit": func(ts time.Time) (string, []any, error) {
			b := chsql.NewBuilder()
			b.DateTime64Lit(ts)
			sql, args, _ := b.Build()
			return sql, args, nil
		},
	}

	for name, build := range constructors {
		for _, zone := range serverTimezones {
			for _, ts := range literalInstants {
				lit, args, err := build(ts)
				if err != nil {
					t.Fatalf("%s: build: %v", name, err)
				}
				query := "SELECT toString(toUnixTimestamp64Nano(" + lit + ")) SETTINGS session_timezone = '" + zone + "'"
				var got string
				if err := db.QueryRow(query, args...).Scan(&got); err != nil {
					t.Fatalf("%s under %s: %v\nquery: %s", name, zone, err, query)
				}
				if want := strconv.FormatInt(ts.UnixNano(), 10); got != want {
					t.Errorf("%s under session_timezone=%s: %s evaluates to %s ns, want %s ns (%s)",
						name, zone, lit, got, want, ts.UTC().Format(time.RFC3339Nano))
				}
			}
		}
	}
}
