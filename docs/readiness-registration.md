# Socket readiness registration

The separate Linux diagnostic profiles from run
[34097519300](https://github.com/brandopakel/keel/actions/runs/34097519300)
attribute 83% of CPU samples to syscalls in the 100k-key workload;
`Epoll.Monitor` accounts for 8.9% cumulatively. The profile was collected
separately from the throughput comparison and does not itself prove an
optimization will improve throughput.

The event loop registered read readiness after every fully flushed reply,
including connections already registered for reads. Both Linux epoll and
Darwin kqueue use level-triggered registrations here: unchanged interest
does not need rearming. Remember the successfully registered operation on
each client and invoke the multiplexer only when it changes. Keep read,
write and append-pause transitions; failed changes are not cached. A new
connection has independent state even when its descriptor number is reused.

The full local Go suite and vet pass, including server and process integration
checks. Hosted race/native/filesystem checks and a matched workload/memory
comparison against the pre-optimization candidate are required before adoption. In
particular, exercise blocked writes, append reply gating, disconnects and
descriptor reuse. No throughput or latency improvement is established yet.
