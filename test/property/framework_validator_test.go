package property

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestValidateOutcomesFailsClosed(t *testing.T) {
	row := Outcome{Rows: []OutcomeRow{{Labels: map[string]string{"job": "api"}, Value: 1}}}
	oracleErr := errors.New("oracle rejected generated shape")
	systemErr := errors.New("query substrate failed")

	tests := []struct {
		name       string
		oracle     Outcome
		system     Outcome
		wantParts  []string
		wantPasses bool
	}{
		{name: "equal rows pass", oracle: row, system: row, wantPasses: true},
		{name: "oracle error", oracle: Outcome{Err: oracleErr}, system: row, wantParts: []string{"oracle error", oracleErr.Error()}},
		{name: "system error", oracle: row, system: Outcome{Err: systemErr}, wantParts: []string{"system error", systemErr.Error()}},
		{
			name:      "matching errors still fail",
			oracle:    Outcome{Err: oracleErr},
			system:    Outcome{Err: systemErr},
			wantParts: []string{"oracle error", oracleErr.Error(), "system error", systemErr.Error()},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateOutcomes(tc.oracle, tc.system)
			if tc.wantPasses {
				if got != "" {
					t.Fatalf("ValidateOutcomes() = %q, want success", got)
				}
				return
			}
			if got == "" {
				t.Fatal("ValidateOutcomes() passed a fail-closed negative control")
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(got, part) {
					t.Errorf("ValidateOutcomes() = %q, want substring %q", got, part)
				}
			}
		})
	}
}

func TestValidateGeneratedQueryFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		query Query
		want  string
	}{
		{name: "valid", query: Query{ShapeID: "test.shape", String: "up"}},
		{name: "missing shape", query: Query{String: "up"}, want: "missing shape ID"},
		{name: "whitespace shape", query: Query{ShapeID: " \t", String: "up"}, want: "missing shape ID"},
		{name: "empty query", query: Query{ShapeID: "test.shape"}, want: "empty query"},
		{name: "whitespace query", query: Query{ShapeID: "test.shape", String: " \t\n"}, want: "empty query"},
		{name: "both missing", want: "missing shape ID; empty query"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidateGeneratedQuery(tc.query); got != tc.want {
				t.Fatalf("ValidateGeneratedQuery() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGeneratedDatasetValidationFailsClosed(t *testing.T) {
	if got := ValidateMetricsDataset(Dataset{}); got != "missing metrics model" {
		t.Fatalf("ValidateMetricsDataset(nil) = %q", got)
	}
	if got := ValidateMetricsDataset(Dataset{Metrics: &MetricsModel{}}); got != "empty metrics series" {
		t.Fatalf("ValidateMetricsDataset(empty) = %q", got)
	}
	if got := ValidateMetricsDataset(Dataset{
		DDL:     "CREATE TABLE metrics",
		Metrics: &MetricsModel{Series: []SeriesData{{MetricName: "up"}}},
	}); got != "metrics dataset has no sample points" {
		t.Fatalf("ValidateMetricsDataset(no points) = %q", got)
	}
	if got := ValidateLogsDataset(Dataset{}); got != "missing logs model" {
		t.Fatalf("ValidateLogsDataset(nil) = %q", got)
	}
	if got := ValidateLogsDataset(Dataset{Logs: &LogsModel{}}); got != "empty log records" {
		t.Fatalf("ValidateLogsDataset(empty) = %q", got)
	}
	validMetrics := Dataset{
		DDL:     "CREATE TABLE metrics",
		Metrics: &MetricsModel{Series: []SeriesData{{MetricName: "up", Points: []Point{{Value: 1}}}}},
	}
	if got := ValidateGeneratedDataset(validMetrics); got != "" {
		t.Fatalf("ValidateGeneratedDataset(valid metrics) = %q", got)
	}
	if got := ValidateGeneratedDataset(Dataset{}); got != "missing metrics or logs model" {
		t.Fatalf("ValidateGeneratedDataset(empty) = %q", got)
	}
	if got := ValidateGeneratedDataset(Dataset{
		DDL:     "seed",
		Metrics: validMetrics.Metrics,
		Logs:    &LogsModel{Records: []LogRecord{{Body: "line"}}},
	}); got != "dataset contains both metrics and logs models" {
		t.Fatalf("ValidateGeneratedDataset(ambiguous) = %q", got)
	}
}

func TestShapeExampleSeedIsStableAndPositionIndependent(t *testing.T) {
	const shapeID ShapeID = "promql.instant.selector"
	const want = 1791708605
	if got := ShapeExampleSeed(shapeID); got != want {
		t.Fatalf("ShapeExampleSeed(%q) = %d, want %d", shapeID, got, want)
	}
	if got := ShapeExampleSeed("promql.instant.sum"); got == want {
		t.Fatalf("distinct shape IDs produced the same enrollment seed %d", got)
	}
}

func TestShapeRosterValidationFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		ids  []ShapeID
		want string
	}{
		{name: "valid", ids: []ShapeID{"test.shape.alpha", "test.shape.beta"}},
		{name: "empty roster", want: "property shape roster is empty"},
		{name: "empty ID", ids: []ShapeID{"test.shape.alpha", ""}, want: "property shape roster contains an empty ID"},
		{name: "whitespace ID", ids: []ShapeID{"test.shape.alpha", " \t"}, want: "property shape roster contains an empty ID"},
		{
			name: "duplicate ID",
			ids:  []ShapeID{"test.shape.alpha", "test.shape.alpha"},
			want: `property shape roster contains duplicate ID "test.shape.alpha"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateShapeRoster(tc.ids); got != tc.want {
				t.Fatalf("validateShapeRoster() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunShapeExamplesRejectsInvalidRosterBeforeCallbacks(t *testing.T) {
	tests := []struct {
		name string
		ids  []ShapeID
		want string
	}{
		{name: "empty roster", want: "property shape roster is empty"},
		{name: "empty ID", ids: []ShapeID{"test.shape.alpha", ""}, want: "property shape roster contains an empty ID"},
		{name: "whitespace ID", ids: []ShapeID{"test.shape.alpha", " \t"}, want: "property shape roster contains an empty ID"},
		{
			name: "duplicate ID",
			ids:  []ShapeID{"test.shape.alpha", "test.shape.alpha"},
			want: `property shape roster contains duplicate ID "test.shape.alpha"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var exampleCalls, oracleCalls, systemCalls int
			got := runShapeExamples(
				t,
				tc.ids,
				func(shapeID ShapeID, _ int) (Dataset, Query) {
					exampleCalls++
					return Dataset{
						DDL:     "CREATE TABLE property_shapes",
						Metrics: &MetricsModel{Series: []SeriesData{{MetricName: "up", Points: []Point{{Value: 1}}}}},
					}, Query{ShapeID: shapeID, String: string(shapeID)}
				},
				func(_ *testing.T, _ Dataset, _ Query) Outcome {
					oracleCalls++
					return Outcome{}
				},
				func(_ *testing.T, _ Dataset, _ Query) Outcome {
					systemCalls++
					return Outcome{}
				},
			)
			if got != tc.want {
				t.Fatalf("runShapeExamples() = %q, want %q", got, tc.want)
			}
			if exampleCalls != 0 || oracleCalls != 0 || systemCalls != 0 {
				t.Fatalf(
					"invalid roster reached callbacks: example=%d oracle=%d system=%d",
					exampleCalls,
					oracleCalls,
					systemCalls,
				)
			}
		})
	}
}

func TestRunShapeCasesRejectsMixedModelDatasetBeforeExecution(t *testing.T) {
	var runCalls int
	got := runShapeCases(
		t,
		[]ShapeID{"test.shape.mixed-model"},
		func(shapeID ShapeID, _ int) (Dataset, Query, ShapeID, string) {
			dataset := Dataset{
				DDL:     "seed",
				Metrics: &MetricsModel{Series: []SeriesData{{MetricName: "up", Points: []Point{{Value: 1}}}}},
				Logs:    &LogsModel{Records: []LogRecord{{Body: "line"}}},
			}
			query := Query{ShapeID: shapeID, String: "up"}
			return dataset, query, query.ShapeID, query.String
		},
		func(_ *testing.T, _ Dataset, _ Query) {
			runCalls++
		},
	)
	want := `shape "test.shape.mixed-model" generated an invalid dataset: dataset contains both metrics and logs models`
	if got != want {
		t.Fatalf("runShapeCases() = %q, want %q", got, want)
	}
	if runCalls != 0 {
		t.Fatalf("mixed-model dataset reached execution callback %d times", runCalls)
	}
}

func TestRunShapeCasesRejectsWhitespaceQueryBeforeExecution(t *testing.T) {
	var exampleCalls, runCalls int
	got := runShapeCases(
		t,
		[]ShapeID{"test.shape.valid", "test.shape.whitespace-query"},
		func(shapeID ShapeID, _ int) (Dataset, Query, ShapeID, string) {
			exampleCalls++
			queryText := string(shapeID)
			if shapeID == "test.shape.whitespace-query" {
				queryText = " \t\n"
			}
			dataset := Dataset{
				DDL:     "seed",
				Metrics: &MetricsModel{Series: []SeriesData{{MetricName: "up", Points: []Point{{Value: 1}}}}},
			}
			query := Query{ShapeID: shapeID, String: queryText}
			return dataset, query, query.ShapeID, query.String
		},
		func(_ *testing.T, _ Dataset, _ Query) {
			runCalls++
		},
	)
	want := `shape "test.shape.whitespace-query" generated an empty query`
	if got != want {
		t.Fatalf("runShapeCases() = %q, want %q", got, want)
	}
	if exampleCalls != 2 {
		t.Fatalf("runShapeCases() built %d examples, want the complete roster of 2", exampleCalls)
	}
	if runCalls != 0 {
		t.Fatalf("whitespace-only query reached execution callback %d times", runCalls)
	}
}

func TestRunShapeExamplesExecutesEachRosterMemberExactlyOnce(t *testing.T) {
	shapeIDs := []ShapeID{"test.shape.alpha", "test.shape.beta"}
	exampleCalls := map[ShapeID]int{}
	oracleCalls := map[ShapeID]int{}
	systemCalls := map[ShapeID]int{}
	oracleTests := map[ShapeID]*testing.T{}
	systemTests := map[ShapeID]*testing.T{}

	RunShapeExamples(
		t,
		shapeIDs,
		func(shapeID ShapeID, _ int) (Dataset, Query) {
			exampleCalls[shapeID]++
			return Dataset{
				DDL:     "CREATE TABLE property_shapes",
				Metrics: &MetricsModel{Series: []SeriesData{{MetricName: "up", Points: []Point{{Value: 1}}}}},
			}, Query{ShapeID: shapeID, String: string(shapeID)}
		},
		func(child *testing.T, _ Dataset, query Query) Outcome {
			oracleCalls[query.ShapeID]++
			oracleTests[query.ShapeID] = child
			return Outcome{Rows: []OutcomeRow{{Value: 1}}}
		},
		func(child *testing.T, _ Dataset, query Query) Outcome {
			systemCalls[query.ShapeID]++
			systemTests[query.ShapeID] = child
			return Outcome{Rows: []OutcomeRow{{Value: 1}}}
		},
	)

	for _, shapeID := range shapeIDs {
		if exampleCalls[shapeID] != 1 || oracleCalls[shapeID] != 1 || systemCalls[shapeID] != 1 {
			t.Errorf("shape %q calls: example=%d oracle=%d system=%d, want exactly one each",
				shapeID, exampleCalls[shapeID], oracleCalls[shapeID], systemCalls[shapeID])
		}
		if oracleTests[shapeID] == nil || oracleTests[shapeID] == t {
			t.Errorf("shape %q oracle did not receive its child *testing.T", shapeID)
		}
		if systemTests[shapeID] != oracleTests[shapeID] {
			t.Errorf("shape %q system and oracle received different subtests", shapeID)
		}
	}
}

func TestRunShapeExamplesAdvancesStableSeedsUntilOracleEvidence(t *testing.T) {
	const (
		shapeID          ShapeID = "test.shape.oracle-guided-search"
		emptyOracleCalls int     = 3
	)
	var (
		exampleSeeds []int
		oracleCalls  int
		systemCalls  int
		systemSeed   int64
	)

	RunShapeExamples(
		t,
		[]ShapeID{shapeID},
		func(generatedShapeID ShapeID, seed int) (Dataset, Query) {
			exampleSeeds = append(exampleSeeds, seed)
			return Dataset{
				DDL: "CREATE TABLE property_shapes",
				Metrics: &MetricsModel{Series: []SeriesData{{
					MetricName: "up",
					Points:     []Point{{Value: float64(seed)}},
				}}},
			}, Query{ShapeID: generatedShapeID, String: "up", EvalTs: int64(seed)}
		},
		func(_ *testing.T, _ Dataset, query Query) Outcome {
			oracleCalls++
			if oracleCalls <= emptyOracleCalls {
				return Outcome{}
			}
			return Outcome{Rows: []OutcomeRow{{Value: float64(query.EvalTs)}}}
		},
		func(_ *testing.T, _ Dataset, query Query) Outcome {
			systemCalls++
			systemSeed = query.EvalTs
			return Outcome{Rows: []OutcomeRow{{Value: float64(query.EvalTs)}}}
		},
	)

	wantAttempts := emptyOracleCalls + 1
	if len(exampleSeeds) != wantAttempts || oracleCalls != wantAttempts {
		t.Fatalf("search calls: examples=%d oracle=%d, want %d each", len(exampleSeeds), oracleCalls, wantAttempts)
	}
	for attempt, got := range exampleSeeds {
		if want := ShapeExampleAttemptSeed(shapeID, attempt); got != want {
			t.Errorf("attempt %d seed = %d, want stable seed %d", attempt, got, want)
		}
	}
	if systemCalls != 1 {
		t.Fatalf("system calls = %d, want exactly 1", systemCalls)
	}
	if want := int64(exampleSeeds[emptyOracleCalls]); systemSeed != want {
		t.Fatalf("system seed = %d, want first non-empty oracle seed %d", systemSeed, want)
	}
}

func TestRunShapeExamplesFailureControls(t *testing.T) {
	const childModeEnv = "CERBERUS_PROPERTY_VALIDATOR_CHILD_MODE"

	switch os.Getenv(childModeEnv) {
	case "oracle-error":
		runShapeExamplesOracleErrorChild(t)
		return
	case "bounded-exhaustion":
		runShapeExamplesBoundedExhaustionChild(t)
		return
	}

	tests := []struct {
		name       string
		mode       string
		wantOutput []string
	}{
		{
			name: "oracle errors fail immediately",
			mode: "oracle-error",
			wantOutput: []string{
				"sentinel oracle failure",
				"oracle-error callbacks: example=1 oracle=1 system=0",
			},
		},
		{
			name: "bounded empty search fails",
			mode: "bounded-exhaustion",
			wantOutput: []string{
				fmt.Sprintf("produced no oracle rows across %d stable attempts", ShapeExampleAttemptLimit),
				fmt.Sprintf("bounded-exhaustion callbacks: example=%d oracle=%d system=0", ShapeExampleAttemptLimit, ShapeExampleAttemptLimit),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestRunShapeExamplesFailureControls$", "-test.v")
			command.Env = append(os.Environ(), childModeEnv+"="+tc.mode)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("failure control %q passed unexpectedly\n%s", tc.mode, output)
			}
			for _, want := range tc.wantOutput {
				if !strings.Contains(string(output), want) {
					t.Fatalf("failure control %q output omitted %q\n%s", tc.mode, want, output)
				}
			}
		})
	}
}

func runShapeExamplesOracleErrorChild(t *testing.T) {
	var exampleCalls, oracleCalls, systemCalls int
	RunShapeExamples(
		t,
		[]ShapeID{"test.shape.oracle-error"},
		func(shapeID ShapeID, _ int) (Dataset, Query) {
			exampleCalls++
			return validShapeExampleDataset(), Query{ShapeID: shapeID, String: "up"}
		},
		func(_ *testing.T, _ Dataset, _ Query) Outcome {
			oracleCalls++
			return Outcome{Err: errors.New("sentinel oracle failure")}
		},
		func(_ *testing.T, _ Dataset, _ Query) Outcome {
			systemCalls++
			return Outcome{Rows: []OutcomeRow{{Value: 1}}}
		},
	)
	if exampleCalls != 1 || oracleCalls != 1 || systemCalls != 0 {
		t.Fatalf("oracle-error callbacks: example=%d oracle=%d system=%d, want 1, 1, 0",
			exampleCalls, oracleCalls, systemCalls)
	}
	t.Logf("oracle-error callbacks: example=%d oracle=%d system=%d", exampleCalls, oracleCalls, systemCalls)
}

func runShapeExamplesBoundedExhaustionChild(t *testing.T) {
	var (
		exampleSeeds []int
		oracleCalls  int
		systemCalls  int
	)
	const shapeID ShapeID = "test.shape.bounded-exhaustion"
	RunShapeExamples(
		t,
		[]ShapeID{shapeID},
		func(generatedShapeID ShapeID, seed int) (Dataset, Query) {
			exampleSeeds = append(exampleSeeds, seed)
			return validShapeExampleDataset(), Query{ShapeID: generatedShapeID, String: "up"}
		},
		func(_ *testing.T, _ Dataset, _ Query) Outcome {
			oracleCalls++
			return Outcome{}
		},
		func(_ *testing.T, _ Dataset, _ Query) Outcome {
			systemCalls++
			return Outcome{Rows: []OutcomeRow{{Value: 1}}}
		},
	)
	if len(exampleSeeds) != ShapeExampleAttemptLimit || oracleCalls != ShapeExampleAttemptLimit || systemCalls != 0 {
		t.Fatalf("bounded-exhaustion callbacks: example=%d oracle=%d system=%d, want %d, %d, 0",
			len(exampleSeeds), oracleCalls, systemCalls, ShapeExampleAttemptLimit, ShapeExampleAttemptLimit)
	}
	for attempt, got := range exampleSeeds {
		if want := ShapeExampleAttemptSeed(shapeID, attempt); got != want {
			t.Fatalf("bounded-exhaustion attempt %d seed = %d, want %d", attempt, got, want)
		}
	}
	t.Logf("bounded-exhaustion callbacks: example=%d oracle=%d system=%d",
		len(exampleSeeds), oracleCalls, systemCalls)
}

func validShapeExampleDataset() Dataset {
	return Dataset{
		DDL:     "CREATE TABLE property_shapes",
		Metrics: &MetricsModel{Series: []SeriesData{{MetricName: "up", Points: []Point{{Value: 1}}}}},
	}
}

func TestValidateDeterministicOutcomesRequiresRowEvidence(t *testing.T) {
	row := Outcome{Rows: []OutcomeRow{{Labels: map[string]string{"job": "api"}, Value: 1}}}
	tests := []struct {
		name       string
		oracle     Outcome
		system     Outcome
		wantPasses bool
		wantPart   string
	}{
		{name: "matching row passes", oracle: row, system: row, wantPasses: true},
		{
			name:     "empty agreement fails",
			wantPart: "deterministic property case produced no rows on either side",
		},
		{
			name:     "row disagreement still fails",
			oracle:   row,
			wantPart: "missing series in got",
		},
		{
			name:     "oracle error still fails",
			oracle:   Outcome{Err: errors.New("unsupported shape")},
			system:   row,
			wantPart: "oracle error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateDeterministicOutcomes(tc.oracle, tc.system)
			if tc.wantPasses {
				if got != "" {
					t.Fatalf("ValidateDeterministicOutcomes() = %q, want success", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantPart) {
				t.Fatalf("ValidateDeterministicOutcomes() = %q, want substring %q", got, tc.wantPart)
			}
		})
	}
}
