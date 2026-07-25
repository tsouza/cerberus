package steps

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// classifyGolden is the committed classification each archetype's ledger is
// compared against.
const classifyGolden = "classify.json"

// registerClassifySteps binds the MIG-03 steps.
func (w *World) registerClassifySteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the operator classifies the corpus offline$`, w.whenClassify)
	ctx.Step(`^each classification matches its committed golden byte for byte$`, w.thenClassifyMatchesGolden)
	ctx.Step(`^every harvested query lands in exactly one bucket$`, w.thenEveryQueryIsBucketed)
	ctx.Step(`^every unsupported query names the construct that rejected it$`, w.thenUnsupportedNamesConstruct)
	ctx.Step(`^every entry the harvester dropped is carried into the classification$`, w.thenClassifyCarriesDrops)
	ctx.Step(`^the bucket counts add up to the number of queries classified$`, w.thenBucketCountsAddUp)
}

// whenClassify buckets each tagged archetype's harvested corpus.
func (w *World) whenClassify() error {
	if err := w.requireArchetypes("the classification"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		corpus, err := w.corpusPathFor(a)
		if err != nil {
			return err
		}
		data, err := w.runArtifact(a, classifyGolden, "migrate", "classify", "--corpus", corpus, "--json")
		if err != nil {
			return err
		}
		var cl migrate.Classification
		if err := lib.DecodeArtifact(data, &cl); err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		w.classifyRaw[a], w.classify[a] = data, cl
	}
	return nil
}

// thenClassifyMatchesGolden compares each ledger against its committed golden,
// having first established that something was actually classified.
func (w *World) thenClassifyMatchesGolden() error {
	return w.eachClassification(func(a string, cl migrate.Classification) error {
		if cl.Counts.Total == 0 {
			return fmt.Errorf("archetype %s classified nothing", a)
		}
		if cl.SchemaVersion != migrate.ClassificationVersion {
			return fmt.Errorf("archetype %s emitted classification version %d, this build reads %d",
				a, cl.SchemaVersion, migrate.ClassificationVersion)
		}
		return lib.AssertGolden(w.goldenPath(a, classifyGolden), w.classifyRaw[a])
	})
}

// thenEveryQueryIsBucketed asserts the ledger covers exactly the corpus it was
// built from — every harvested query appears, none appears twice, and each
// carries one of the two declared buckets. A query that fell out of the ledger
// between harvest and classification is one nobody ever decided about.
func (w *World) thenEveryQueryIsBucketed() error {
	return w.eachClassification(func(a string, cl migrate.Classification) error {
		classified := map[string]string{}
		for _, q := range cl.Queries {
			switch q.Bucket {
			case migrate.BucketSupported, migrate.BucketUnsupported:
			default:
				return fmt.Errorf("archetype %s: query from %s carries bucket %q, which is neither %q nor %q",
					a, q.Source, q.Bucket, migrate.BucketSupported, migrate.BucketUnsupported)
			}
			key := q.Source + "\x00" + q.Expr
			if prev, dup := classified[key]; dup {
				return fmt.Errorf("archetype %s: %s from %s is bucketed twice (%s and %s)",
					a, q.Expr, q.Source, prev, q.Bucket)
			}
			classified[key] = q.Bucket
		}
		for _, q := range w.corpus[a].Queries {
			if _, ok := classified[q.Source+"\x00"+q.Expr]; !ok {
				return fmt.Errorf("archetype %s: harvested query %q from %s never reached a bucket", a, q.Expr, q.Source)
			}
		}
		if len(classified) != len(w.corpus[a].Queries) {
			return fmt.Errorf("archetype %s classified %d distinct queries from a corpus of %d",
				a, len(classified), len(w.corpus[a].Queries))
		}
		return nil
	})
}

// thenUnsupportedNamesConstruct asserts every rejected query names what
// rejected it, and that the tagged set actually contains rejections. A ledger
// with no unsupported entry would satisfy the per-entry check while never
// reading the field the operator rewrites against.
func (w *World) thenUnsupportedNamesConstruct() error {
	var unsupported int
	if err := w.eachClassification(func(a string, cl migrate.Classification) error {
		var counted int
		for _, q := range cl.Queries {
			if q.Bucket != migrate.BucketUnsupported {
				if q.Construct != "" {
					return fmt.Errorf("archetype %s: supported query from %s names a rejecting construct %q",
						a, q.Source, q.Construct)
				}
				continue
			}
			counted++
			if strings.TrimSpace(q.Construct) == "" {
				return fmt.Errorf("archetype %s: unsupported query %q from %s names no rejecting construct",
					a, q.Expr, q.Source)
			}
		}
		if counted != cl.Counts.Unsupported {
			return fmt.Errorf("archetype %s counts %d unsupported queries but lists %d",
				a, cl.Counts.Unsupported, counted)
		}
		unsupported += counted
		return nil
	}); err != nil {
		return err
	}
	if unsupported == 0 {
		return fmt.Errorf(
			"no tagged archetype produced an unsupported query, so the rejection reporting was never exercised; %v classify clean",
			w.archetypes,
		)
	}
	return nil
}

// thenClassifyCarriesDrops asserts the harvester's drops reach the ledger
// unchanged. A skipped corpus entry is a query nobody classified, so losing it
// here would hide it behind a clean-looking bucket tally.
func (w *World) thenClassifyCarriesDrops() error {
	var dropped int
	if err := w.eachClassification(func(a string, cl migrate.Classification) error {
		want := w.corpus[a].Skipped
		if !reflect.DeepEqual(cl.Skipped, want) {
			return fmt.Errorf("archetype %s: the ledger carries %+v, the harvest dropped %+v", a, cl.Skipped, want)
		}
		dropped += len(cl.Skipped)
		return requireStatedReasons(a, "classification skipped", cl.Skipped)
	}); err != nil {
		return err
	}
	if dropped == 0 {
		return fmt.Errorf(
			"no tagged archetype dropped an input, so carrying the drops through was never exercised; %v harvest cleanly",
			w.archetypes,
		)
	}
	return nil
}

// thenBucketCountsAddUp asserts the headline tally is arithmetically honest:
// the two buckets partition the total, the total is the number of listed
// queries, and the risky flag stays a subset of supported rather than becoming
// a silent third bucket.
func (w *World) thenBucketCountsAddUp() error {
	return w.eachClassification(func(a string, cl migrate.Classification) error {
		c := cl.Counts
		if c.Supported+c.Unsupported != c.Total {
			return fmt.Errorf("archetype %s: %d supported + %d unsupported does not equal the total of %d",
				a, c.Supported, c.Unsupported, c.Total)
		}
		if len(cl.Queries) != c.Total {
			return fmt.Errorf("archetype %s: the ledger lists %d queries but counts %d", a, len(cl.Queries), c.Total)
		}
		if c.Risky > c.Supported {
			return fmt.Errorf("archetype %s: %d risky queries exceed the %d supported ones they must be drawn from",
				a, c.Risky, c.Supported)
		}
		var risky int
		for _, q := range cl.Queries {
			if q.Risky {
				risky++
				if q.Bucket != migrate.BucketSupported {
					return fmt.Errorf("archetype %s: query from %s is flagged risky outside the supported bucket", a, q.Source)
				}
				if len(q.Risks) == 0 {
					return fmt.Errorf("archetype %s: query from %s is flagged risky but lists no risk", a, q.Source)
				}
			}
		}
		if risky != c.Risky {
			return fmt.Errorf("archetype %s counts %d risky queries but flags %d", a, c.Risky, risky)
		}
		return nil
	})
}

// eachClassification runs fn over every tagged archetype's ledger, failing when
// a When never produced one.
func (w *World) eachClassification(fn func(archetype string, cl migrate.Classification) error) error {
	if err := w.requireArchetypes("the classification assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		cl, ok := w.classify[a]
		if !ok {
			return fmt.Errorf("archetype %s has no classification; the scenario must classify first", a)
		}
		if err := fn(a, cl); err != nil {
			return err
		}
	}
	return nil
}
