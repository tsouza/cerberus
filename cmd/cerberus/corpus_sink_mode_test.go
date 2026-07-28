package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/config"
)

// errCorpusSinkDDL is the failure a CH-table sink construction hits on a
// deployment whose CH user cannot run the corpus DDL.
var errCorpusSinkDDL = errors.New("not enough privileges")

// failingCorpusConn is an optcorpus.CHTableConn whose every statement fails,
// standing in for the least-privilege deployment: the sink cannot be built.
type failingCorpusConn struct{}

func (failingCorpusConn) Exec(context.Context, string, ...any) error { return errCorpusSinkDDL }

func (failingCorpusConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return nil, errCorpusSinkDDL
}

func (failingCorpusConn) PrepareBatch(context.Context, string, ...driver.PrepareBatchOption) (driver.Batch, error) {
	return nil, errCorpusSinkDDL
}

// TestBuildCorpusSink_CHTableFailureDisablesReconciler pins the sink-selection
// contract the CH-table reconciliation depends on: the CONFIGURED mode is
// honoured or nothing is.
//
// The temptation is to treat a chtable failure as a reason to write the JSONL
// file instead, since a sink path may well be configured. That would tell an
// operator who asked for the CH table that the corpus is healthy while every
// consumer of cerberus_router_corpus reads an empty table. The reconciler stays
// off instead, and the startup log says so. This test pins that decision with a
// SinkPath deliberately set, because the tempting-but-wrong behaviour is
// distinguishable only in exactly that configuration.
func TestBuildCorpusSink_CHTableFailureDisablesReconciler(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.CHOptCorpus.SinkMode = corpusSinkModeCHTable
	cfg.CHOptCorpus.SinkPath = filepath.Join(t.TempDir(), "corpus.jsonl")

	sink, desc, ok := buildCorpusSink(context.Background(), discardLogger(), failingCorpusConn{}, cfg)
	if ok {
		t.Fatalf("chtable sink construction failed but buildCorpusSink reported ok (desc %q)", desc)
	}
	if sink != nil {
		t.Errorf("sink = %#v; a failed construction must return no sink", sink)
	}
	if desc != "" {
		t.Errorf("sink description = %q; want empty on failure", desc)
	}
}

// TestBuildCorpusSink_JSONLModeIgnoresCHFailure is the other half of the
// contract: in jsonl mode the CH connection is never consulted, so a CH that
// rejects every statement does not disable the reconciler.
func TestBuildCorpusSink_JSONLModeIgnoresCHFailure(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.CHOptCorpus.SinkPath = filepath.Join(t.TempDir(), "corpus.jsonl")

	sink, desc, ok := buildCorpusSink(context.Background(), discardLogger(), failingCorpusConn{}, cfg)
	if !ok {
		t.Fatalf("jsonl sink must build without touching ClickHouse")
	}
	t.Cleanup(func() { _ = sink.Close() })
	if want := "jsonl:" + cfg.CHOptCorpus.SinkPath; desc != want {
		t.Errorf("sink description = %q; want %q", desc, want)
	}
}
