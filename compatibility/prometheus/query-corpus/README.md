# Prometheus compatibility query corpus

`header.yml` and the files listed by `manifest.txt` form one YAML document for the upstream
PromQL compliance tester. Fragments own independent query families; their numeric prefixes and
manifest order are canonical.

The `querycorpus` loader fails closed when a listed fragment is absent, an unlisted file is present,
the order or numbering changes, a fragment is empty or malformed, or a complete test case is
duplicated. `cmd/assemble-queries` materializes the validated document for the live compatibility
harness. `test/regression/compat_promql_seed_corpus_test.go` uses the same loader, so the seed
coverage gate and live tester cannot silently consume different corpora.

Add a case to the narrowest existing family fragment. Add a new fragment only when no existing
family owns it; assign the next contiguous prefix and add the same filename to `manifest.txt`.
