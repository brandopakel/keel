# Sparse HLL heap measurement

The broad race-instrumented serialization/HLL subset failed the existing heap
estimate test on unchanged develop 7fc360d9: the one-item case estimated 160,000
bytes but measured 119,088. The same test passed in isolation on baseline and
dump candidate. This reproduces a test-order sensitivity independent of the
dump change; process-wide HeapAlloc deltas can include unrelated GC cleanup.

The empty and one-item populations now contain 20,000 objects instead of 2,000,
so their retained payload exceeds 1 MiB. The existing ±15% tolerance, memory
estimate formulas and larger-cardinality populations are unchanged. Three broad
race-instrumented repetitions pass, and normal isolated estimates/heap ratios
are 0.999, 1.003, 1.003 and 1.000 for 0/1/128/800 items. This improves the signal
relative to unrelated heap noise; it is not a Keel memory optimization. All
attempts are retained in bench/results/heap-measurement-2026-09-07.json.gz.
