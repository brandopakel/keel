# Keel development and validation

- Keep local work small. Use GitHub Actions for workload matrices, full race and
  platform suites, capacity testing, extended fuzzing and operational soaks. Do
  not start detached or long-running local tests, including a 48-hour soak.
- Local checks must be brief (at most two minutes) and use
  `scripts/run-local-validation.py`, with fresh output below `dist/`. The wrapper
  enforces time, file-size and free-space limits and stops its owned children.
  Its default output budget is 512 MiB. Use a disposable Go cache inside its
  output directory when compilation is needed; remove it after preserving results.
- Publish compact reports, provenance, checksums and failure evidence to GitHub
  before pruning local results. Keep successful disposable persistence files only
  until their checksum/report is recorded. Do not commit raw multi-gigabyte AOFs,
  build caches, binaries, environment files, credentials or personal machine data.
- Remove obsolete clean worktrees after verifying their commits and any unique
  evidence are saved remotely. Preserve uncommitted work and active workspaces.
- Do not modify GoGIF or other projects for a Keel benchmark. Use identical
  workloads and durability settings for baseline and candidate comparisons.
- The budget is $0. Use the already authorized free public GitHub runners. Do not
  provision paid resources or upgrade review/benchmark services.
- Record failures alongside successful repeats. A stopped or partial soak is not
  a pass; a passing earlier build does not validate a later runtime.
