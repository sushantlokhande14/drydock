# The sandbox image: everything an agent needs to work on a Python repository,
# installed once at build time so that no run pays for it and no run needs the
# network to get it.
FROM python:3.11-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ripgrep patch \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir pytest==8.3.4

# Nothing in a sandbox runs as root. The uid is fixed because the driver names it.
RUN useradd --create-home --uid 1000 agent \
 && mkdir -p /workspace /cache \
 && chown agent:agent /workspace /cache

# System-wide git settings, because a sandbox's home directory is an empty tmpfs.
# safe.directory: the repository cache volume is written by one container and read by another.
RUN git config --system --add safe.directory '*' \
 && git config --system user.name "drydock" \
 && git config --system user.email "drydock@localhost" \
 && git config --system advice.detachedHead false \
 && git config --system init.defaultBranch main

USER agent
WORKDIR /workspace
CMD ["sleep", "infinity"]
