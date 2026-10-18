//go:build chdb

package chdbsession_test

import (
	"database/sql"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chdb-io/chdb-go/chdb"
	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chdbsession"
)

// perQueryTimezone is a zone with no daylight saving and a whole-hour offset
// from UTC, so a literal read in it lands a fixed, nonzero distance away.
const perQueryTimezone = "Asia/Tokyo"

// hostTimezone is the zone the re-executed child runs under, standing in for
// a non-UTC developer host. It differs from both UTC and perQueryTimezone.
const hostTimezone = "America/Sao_Paulo"

// childEnv marks the re-executed child, which only runs the in-process checks.
const childEnv = "CHDBSESSION_TIMEZONE_CHILD"

// childPassMarker is what the child's verbose output carries when the test ran
// and passed.
const childPassMarker = "--- PASS: TestServerTimezoneIsPinnedToUTC"

// zonelessLiteral is a seed-style DateTime literal with no zone of its own.
const zonelessLiteral = "2026-05-14 12:00:00"

// serverZoneQuery reads the session and server timezones and the instant the
// server reads a zone-less literal as.
const serverZoneQuery = "SELECT timezone(), serverTimezone(), toUnixTimestamp(toDateTime('" + zonelessLiteral + "'))"

type serverZone struct {
	session, server string
	unix            int64
}

func readServerZone(t *testing.T, query string) serverZone {
	t.Helper()

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	var z serverZone
	if err := db.QueryRow(query).Scan(&z.session, &z.server, &z.unix); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return z
}

// TestServerTimezoneIsPinnedToUTC pins that the embedded ClickHouse reads a
// zone-less literal as UTC whatever the host's timezone, that a rebuilt
// session keeps the pin, and that a per-query session_timezone still
// overrides it.
//
// CI runners are UTC, where an unpinned server reads the literal as UTC too,
// so the test also re-executes itself under TZ=hostTimezone: that child is
// the non-UTC host, and it fails without the pin on any machine.
func TestServerTimezoneIsPinnedToUTC(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Run("non-UTC host", func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestServerTimezoneIsPinnedToUTC$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), "TZ="+hostTimezone, childEnv+"=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the checks failed in a child running under TZ=%s: %v\n%s", hostTimezone, err, out)
			}
			if !strings.Contains(string(out), childPassMarker) {
				t.Fatalf("the child under TZ=%s did not report %q, so it ran no check:\n%s",
					hostTimezone, childPassMarker, out)
			}
		})
	}

	want, err := time.Parse(time.DateTime, zonelessLiteral)
	if err != nil {
		t.Fatalf("parse %q: %v", zonelessLiteral, err)
	}

	check := func(t *testing.T) {
		t.Helper()
		got := readServerZone(t, serverZoneQuery)
		if got.session != chdbsession.ServerTimezone || got.server != chdbsession.ServerTimezone {
			t.Errorf("timezone()=%q serverTimezone()=%q, want both %q: the embedded server took the "+
				"host's zone, so a zone-less seed literal lands at a host-dependent instant",
				got.session, got.server, chdbsession.ServerTimezone)
		}
		if got.unix != want.Unix() {
			t.Errorf("toDateTime(%q) = %d, want %d (the literal read as UTC)", zonelessLiteral, got.unix, want.Unix())
		}
	}

	t.Run("first session", check)

	t.Run("rebuilt session", func(t *testing.T) {
		s, err := chdb.NewSession()
		if err != nil {
			t.Fatalf("open session: %v", err)
		}
		s.Close()
		check(t)
	})

	t.Run("per-query session_timezone overrides the pin", func(t *testing.T) {
		loc, err := time.LoadLocation(perQueryTimezone)
		if err != nil {
			t.Fatalf("load %s: %v", perQueryTimezone, err)
		}
		local, err := time.ParseInLocation(time.DateTime, zonelessLiteral, loc)
		if err != nil {
			t.Fatalf("parse %q in %s: %v", zonelessLiteral, perQueryTimezone, err)
		}
		got := readServerZone(t, serverZoneQuery+" SETTINGS session_timezone = '"+perQueryTimezone+"'")
		if got.session != perQueryTimezone {
			t.Errorf("timezone() = %q under SETTINGS session_timezone = %q: the pin masks a per-query "+
				"override, so no test can observe a non-UTC server", got.session, perQueryTimezone)
		}
		if got.unix != local.Unix() {
			t.Errorf("toDateTime(%q) = %d under session_timezone %s, want %d", zonelessLiteral, got.unix,
				perQueryTimezone, local.Unix())
		}
	})
}
