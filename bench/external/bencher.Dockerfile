# Pinned Linux amd64 Python image; the final image digest identifies installed OS packages.
FROM python:3.12-slim-bookworm@sha256:9c47360a2a0355e2da18516d0b1c2126ec22c195d2185e97347c9d98398c5bef
RUN apt-get update && apt-get install -y --no-install-recommends procps \
    && dpkg-query -W > /opt/keel-packages.txt \
    && rm -rf /var/lib/apt/lists/*
COPY bin/ /opt/keel/bin/
COPY bench/ /opt/keel/bench/
ARG HARNESS_REVISION
ENV KEEL_HARNESS_REVISION=$HARNESS_REVISION
ENV PATH="/opt/keel/bin:${PATH}"
# go version -m is self-contained, but the trimmed Go launcher needs an existing root.
ENV GOROOT=/opt/keel GOTOOLCHAIN=local
WORKDIR /opt/keel
ENTRYPOINT ["python3", "/opt/keel/bench/external/bencher_job.py"]
