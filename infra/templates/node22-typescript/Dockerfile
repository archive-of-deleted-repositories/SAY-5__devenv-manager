# node22-typescript template — Node.js 22 with global pnpm and ts-node
# preinstalled. Used by /v1/images/build to produce a session-ready
# image for TypeScript work.
#
# Build args:
#   PNPM_VERSION  pnpm version to install globally (default: 9)
#   TSNODE_VERSION ts-node version (default: latest)
ARG PNPM_VERSION=9
ARG TSNODE_VERSION=latest

FROM node:22-alpine

ARG PNPM_VERSION
ARG TSNODE_VERSION

# Install build essentials for native modules and a sane shell.
RUN apk add --no-cache bash git ca-certificates

# Global pnpm + ts-node.
RUN npm install -g "pnpm@${PNPM_VERSION}" "ts-node@${TSNODE_VERSION}" typescript

# Workspace dir — the manager mounts /workspace as tmpfs by default.
WORKDIR /workspace

# Default to bash for interactive sessions; the relay's exec already
# handles spawning a shell.
CMD ["bash"]
