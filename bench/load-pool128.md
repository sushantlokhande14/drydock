
Load: 128 scripted agents, up to 128 at once, pool of 128, 2000 ms scripted think time per turn

| Measure | Value |
|---|---:|
| Runs that produced a verified patch | 128 of 128 |
| Peak runs executing at once | 128 |
| Peak sandbox containers running at once | 287 |
| Wall time | 41.3 s |
| Throughput | 186 runs/min |
| Events written | 4957 |
| Startup, all runs (p50 / p95 / max) | 1822 / 2957 / 3632 ms |
| Startup, warm from the pool (128 runs) | 1822 / 2957 / 3632 ms |
| Startup, created on demand (0 runs) | 0 / 0 / 0 ms |
| Agent loop (p50 / p95) | 17363 / 20065 ms |
| Verification in a second sandbox (p50 / p95) | 16276 / 19958 ms |
| Whole run (p50 / p95) | 40092 / 41026 ms |

Outcomes: patch_verified: 128
