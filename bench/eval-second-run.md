
Evaluation: ollama:qwen2.5:7b resolved 2 of 8 attempts (8 tasks x 1)

| Task | Result | Why | Model turns | Tool calls | Seconds |
|---|---|---|---:|---:|---:|
| amounts-parentheses | failed | verification_failed | 18 | 15 | 87 |
| intervals-unsorted-input | succeeded | patch_verified | 13 | 10 | 98 |
| lru-read-recency | succeeded | patch_verified | 25 | 24 | 119 |
| pagination-last-page | failed | model_stopped_calling_tools | 6 | 49 | 193 |
| ratelimit-lost-refill | failed | harness_error | 25 | 25 | 489 |
| slug-stray-hyphens | failed | verification_failed | 24 | 21 | 166 |
| unique-ignores-key | failed | verification_failed | 3 | 7 | 17 |
| versions-string-compare | failed | verification_failed | 12 | 9 | 92 |
