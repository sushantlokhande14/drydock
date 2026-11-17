
Startup: time until a command can run against a checkout of https://github.com/pallets/click

| Arm | What it does | Samples | p50 ms | p95 ms | Max ms | Container ms | Repository ms |
|---|---|---:|---:|---:|---:|---:|---:|
| naive | base image; install git and the test runner; clone over the network | 5 | 13719 | 15383 | 15383 | 228 | 13620 |
| prebuilt image | tools baked into the image; clone over the network | 5 | 1499 | 1714 | 1714 | 188 | 1337 |
| + repository cache | hardened sandbox; clone from the local mirror, sharing its objects | 20 | 159 | 165 | 183 | 110 | 51 |
| + warm pool | sandbox already started and waiting; clone from the local mirror | 20 | 60 | 68 | 70 | 0 | 61 |

Container ms and Repository ms are means of the two parts of each sample.
