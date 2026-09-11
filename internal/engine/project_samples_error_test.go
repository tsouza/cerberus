package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
)

func TestEngineProjectSamplesErrorFailsBeforeEmitOrExecute(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("untrusted sample schema")
	client := &fakeQuerier{}
	lang := &fakeLang{
		name: "promql",
		projectFn: func(chplan.Node, engine.Meta) (chplan.Node, error) {
			return nil, wantErr
		},
	}
	eng := newEngine(client)

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{
			name: "dry_run",
			run: func() error {
				_, err := eng.DryRunSQL(context.Background(), lang, "up")
				return err
			},
		},
		{
			name: "query_plan",
			run: func() error {
				_, err := eng.QueryPlan(context.Background(), lang, &chplan.OneRow{}, engine.Meta{})
				return err
			},
		},
		{
			name: "query_plan_cursor",
			run: func() error {
				_, err := eng.QueryPlanCursor(context.Background(), lang, &chplan.OneRow{}, engine.Meta{})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want projection failure", err)
			}
		})
	}
	if client.calls != 0 {
		t.Fatalf("client calls = %d, want 0", client.calls)
	}
}
