# Scheduled capacity sweep: all measured rates

Run [34108229947](https://github.com/brandopakel/keel/actions/runs/34108229947).
Two repetitions × ten seconds, AOF off, fresh servers, 32 connections per workload;
three tenant cases split connections and use independent queues. Values are medians.
Tenant latency columns take the worst tenant p99 within each repetition, then the median;
they are not merged quantiles. Drops are generator queue admission drops.
Scheduled latency includes generator delay, and describes completed requests only;
the separate drop column is essential. Generator cores are CPU-seconds / ten seconds.
Two exposed generator cores were available; near-two means generator saturation.

| Workload | Offered/s | Arm | Completed/s | Dropped % | Scheduled p99 ms | Service p99 ms | Scheduler p99 ms | Generator cores |
| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| read-64 | 1,000 | baseline | 1,000 | 0.000 | 1.163 | 0.114 | 1.065 | 0.094 |
| read-64 | 1,000 | candidate | 1,000 | 0.000 | 1.163 | 0.115 | 1.065 | 0.089 |
| read-64 | 10,000 | baseline | 10,000 | 0.000 | 1.245 | 0.182 | 1.081 | 0.328 |
| read-64 | 10,000 | candidate | 10,000 | 0.000 | 1.245 | 0.181 | 1.081 | 0.326 |
| read-64 | 50,000 | baseline | 50,000 | 0.000 | 0.946 | 0.326 | 0.444 | 1.275 |
| read-64 | 50,000 | candidate | 50,000 | 0.000 | 0.979 | 0.338 | 0.500 | 1.269 |
| read-64 | 100,000 | baseline | 98,185 | 1.815 | 4.260 | 0.700 | 1.843 | 1.362 |
| read-64 | 100,000 | candidate | 99,190 | 0.810 | 3.654 | 0.684 | 1.139 | 1.435 |
| read-64 | 150,000 | baseline | 100,179 | 33.214 | 4.555 | 0.680 | 1.925 | 1.353 |
| read-64 | 150,000 | candidate | 106,474 | 29.018 | 4.522 | 0.688 | 2.064 | 1.426 |
| balanced-1k | 1,000 | baseline | 1,000 | 0.000 | 1.163 | 0.119 | 1.065 | 0.096 |
| balanced-1k | 1,000 | candidate | 1,000 | 0.000 | 1.163 | 0.118 | 1.065 | 0.090 |
| balanced-1k | 10,000 | baseline | 10,000 | 0.000 | 1.245 | 0.186 | 1.081 | 0.338 |
| balanced-1k | 10,000 | candidate | 10,000 | 0.000 | 1.245 | 0.188 | 1.081 | 0.336 |
| balanced-1k | 50,000 | baseline | 50,000 | 0.000 | 0.942 | 0.375 | 0.268 | 1.281 |
| balanced-1k | 50,000 | candidate | 50,000 | 0.000 | 0.836 | 0.354 | 0.155 | 1.287 |
| balanced-1k | 100,000 | baseline | 96,359 | 3.641 | 4.620 | 0.733 | 1.917 | 1.348 |
| balanced-1k | 100,000 | candidate | 99,198 | 0.802 | 3.670 | 0.713 | 1.253 | 1.434 |
| balanced-1k | 150,000 | baseline | 96,310 | 35.793 | 4.784 | 0.729 | 2.015 | 1.342 |
| balanced-1k | 150,000 | candidate | 101,312 | 32.459 | 4.981 | 0.799 | 2.146 | 1.381 |
| expiry-storm | 1,000 | baseline | 1,000 | 0.000 | 1.171 | 0.120 | 1.073 | 0.095 |
| expiry-storm | 1,000 | candidate | 1,000 | 0.000 | 1.163 | 0.121 | 1.081 | 0.099 |
| expiry-storm | 10,000 | baseline | 10,000 | 0.000 | 1.262 | 0.200 | 1.081 | 0.349 |
| expiry-storm | 10,000 | candidate | 10,000 | 0.000 | 1.262 | 0.197 | 1.081 | 0.344 |
| expiry-storm | 50,000 | baseline | 50,000 | 0.000 | 0.500 | 0.313 | 0.058 | 1.285 |
| expiry-storm | 50,000 | candidate | 50,000 | 0.000 | 0.492 | 0.330 | 0.056 | 1.287 |
| expiry-storm | 100,000 | baseline | 95,311 | 4.689 | 4.620 | 0.700 | 1.933 | 1.319 |
| expiry-storm | 100,000 | candidate | 99,049 | 0.951 | 3.916 | 0.700 | 1.401 | 1.404 |
| expiry-storm | 150,000 | baseline | 94,326 | 37.116 | 4.653 | 0.709 | 1.950 | 1.315 |
| expiry-storm | 150,000 | candidate | 100,268 | 33.154 | 4.588 | 0.721 | 1.999 | 1.376 |
| large-1m | 1,000 | baseline | 1,000 | 0.000 | 2.785 | 2.212 | 1.073 | 0.420 |
| large-1m | 1,000 | candidate | 1,000 | 0.000 | 2.785 | 2.245 | 1.073 | 0.421 |
| large-1m | 10,000 | baseline | 2,343 | 76.573 | 173.015 | 58.458 | 1.262 | 0.815 |
| large-1m | 10,000 | candidate | 2,366 | 76.345 | 171.966 | 57.934 | 1.253 | 0.820 |
| large-1m | 50,000 | baseline | 2,322 | 95.356 | 177.209 | 59.769 | 1.090 | 0.842 |
| large-1m | 50,000 | candidate | 2,299 | 95.402 | 178.258 | 60.031 | 1.081 | 0.834 |
| large-1m | 100,000 | baseline | 2,348 | 97.652 | 174.064 | 58.720 | 1.114 | 0.870 |
| large-1m | 100,000 | candidate | 2,359 | 97.641 | 174.064 | 58.982 | 1.073 | 0.881 |
| large-1m | 150,000 | baseline | 2,388 | 98.408 | 169.869 | 57.934 | 1.065 | 0.998 |
| large-1m | 150,000 | candidate | 2,331 | 98.446 | 173.015 | 59.245 | 1.073 | 1.006 |
| large-hash | 1,000 | baseline | 1,000 | 0.000 | 7.635 | 6.488 | 2.064 | 1.150 |
| large-hash | 1,000 | candidate | 1,000 | 0.000 | 7.242 | 6.128 | 2.015 | 1.138 |
| large-hash | 10,000 | baseline | 1,450 | 85.501 | 245.367 | 58.196 | 25.690 | 1.998 |
| large-hash | 10,000 | candidate | 1,439 | 85.607 | 249.561 | 61.080 | 27.394 | 1.993 |
| large-hash | 50,000 | baseline | 1,442 | 97.116 | 250.610 | 57.934 | 30.409 | 1.996 |
| large-hash | 50,000 | candidate | 1,446 | 97.109 | 250.610 | 57.672 | 29.622 | 1.998 |
| large-hash | 100,000 | baseline | 1,435 | 98.565 | 252.707 | 58.196 | 30.278 | 1.997 |
| large-hash | 100,000 | candidate | 1,437 | 98.563 | 253.755 | 58.458 | 30.409 | 1.996 |
| large-hash | 150,000 | baseline | 1,433 | 99.045 | 252.707 | 58.458 | 29.622 | 1.994 |
| large-hash | 150,000 | candidate | 1,432 | 99.045 | 252.707 | 58.720 | 28.705 | 1.996 |
| large-list | 1,000 | baseline | 1,000 | 0.000 | 2.802 | 1.950 | 1.081 | 0.684 |
| large-list | 1,000 | candidate | 1,000 | 0.000 | 2.703 | 1.802 | 1.081 | 0.680 |
| large-list | 10,000 | baseline | 2,713 | 72.873 | 134.218 | 33.161 | 19.923 | 1.968 |
| large-list | 10,000 | candidate | 2,733 | 72.672 | 132.121 | 32.506 | 19.137 | 1.973 |
| large-list | 50,000 | baseline | 2,715 | 94.570 | 136.315 | 32.375 | 19.530 | 1.973 |
| large-list | 50,000 | candidate | 2,727 | 94.545 | 135.266 | 32.506 | 18.743 | 1.975 |
| large-list | 100,000 | baseline | 2,699 | 97.301 | 136.315 | 31.588 | 19.399 | 1.975 |
| large-list | 100,000 | candidate | 2,705 | 97.295 | 137.363 | 32.637 | 19.530 | 1.976 |
| large-list | 150,000 | baseline | 2,699 | 98.201 | 136.315 | 32.899 | 19.399 | 1.977 |
| large-list | 150,000 | candidate | 2,704 | 98.197 | 138.412 | 33.030 | 19.923 | 1.978 |
| mixed-tenants | 1,000 | baseline | 1,000 | 0.000 | 1.221 | 0.182 | 1.106 | 0.103 |
| mixed-tenants | 1,000 | candidate | 1,000 | 0.000 | 1.221 | 0.182 | 1.098 | 0.104 |
| mixed-tenants | 10,000 | baseline | 10,000 | 0.000 | 1.229 | 0.190 | 1.090 | 0.408 |
| mixed-tenants | 10,000 | candidate | 10,000 | 0.000 | 1.229 | 0.185 | 1.106 | 0.398 |
| mixed-tenants | 50,000 | baseline | 49,997 | 0.005 | 1.409 | 0.410 | 1.171 | 1.479 |
| mixed-tenants | 50,000 | candidate | 49,991 | 0.018 | 1.376 | 0.391 | 1.155 | 1.471 |
| mixed-tenants | 100,000 | baseline | 82,228 | 17.771 | 2.523 | 0.754 | 1.319 | 1.892 |
| mixed-tenants | 100,000 | candidate | 87,979 | 12.021 | 2.343 | 0.877 | 1.622 | 1.881 |
| mixed-tenants | 150,000 | baseline | 81,444 | 45.704 | 3.326 | 0.877 | 1.303 | 1.896 |
| mixed-tenants | 150,000 | candidate | 86,312 | 42.459 | 3.064 | 0.950 | 1.516 | 1.921 |
