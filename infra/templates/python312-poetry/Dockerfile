# python312-poetry template — Python 3.12 with poetry preinstalled.
#
# Build args:
#   POETRY_VERSION  poetry version (default: 1.8.3)
ARG POETRY_VERSION=1.8.3

FROM python:3.12-slim

ARG POETRY_VERSION

# System packages: build essentials for native wheels + git for VCS deps.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential git ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# Install poetry into its own venv to avoid polluting the system Python.
ENV POETRY_HOME=/opt/poetry \
    POETRY_VIRTUALENVS_CREATE=false \
    PATH=/opt/poetry/bin:$PATH
RUN curl -sSL https://install.python-poetry.org | POETRY_VERSION="${POETRY_VERSION}" python3 -

WORKDIR /workspace

CMD ["bash"]
