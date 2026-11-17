
Load: 128 scripted agents, up to 128 at once, pool of 16, 2000 ms scripted think time per turn

| Measure | Value |
|---|---:|
| Runs that produced a verified patch | 128 of 128 |
| Peak runs executing at once | 128 |
| Peak sandbox containers running at once | 180 |
| Wall time | 50.1 s |
| Throughput | 153 runs/min |
| Events written | 4955 |
| Startup, all runs (p50 / p95 / max) | 11292 / 13008 / 13235 ms |
| Startup, warm from the pool (16 runs) | 292 / 484 / 484 ms |
| Startup, created on demand (112 runs) | 11771 / 13011 / 13235 ms |
| Agent loop (p50 / p95) | 17118 / 19735 ms |
| Verification in a second sandbox (p50 / p95) | 14253 / 17686 ms |
| Whole run (p50 / p95) | 40159 / 49585 ms |

Outcomes: patch_verified: 128
