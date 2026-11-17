
Evaluation: ollama:qwen2.5:7b resolved 1 of 8 attempts (8 tasks x 1)

| Task | Result | Why | Model turns | Tool calls | Seconds |
|---|---|---|---:|---:|---:|
| amounts-parentheses | failed | verification_failed | 18 | 15 | 92 |
| intervals-unsorted-input | succeeded | patch_verified | 13 | 10 | 95 |
| lru-read-recency | failed | verification_failed | 25 | 23 | 156 |
| pagination-last-page | failed | model_stopped_calling_tools | 6 | 49 | 172 |
| ratelimit-lost-refill | failed | verification_failed | 25 | 25 | 466 |
| slug-stray-hyphens | failed | verification_failed | 18 | 15 | 90 |
| unique-ignores-key | failed | verification_failed | 3 | 7 | 16 |
| versions-string-compare | failed | verification_failed | 12 | 9 | 86 |
