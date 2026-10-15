FROM node:22-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      git ripgrep python3 python3-pip python3-venv ca-certificates procps \
 && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /workspace && chown node:node /workspace

ENV HOME=/tmp/home \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

USER node
WORKDIR /workspace
CMD ["sleep", "infinity"]
