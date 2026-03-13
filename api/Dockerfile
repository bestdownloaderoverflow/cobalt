FROM node:20-alpine

WORKDIR /app

# Install pnpm and dependencies for native modules
RUN corepack enable && corepack prepare pnpm@latest --activate
RUN apk add --no-cache python3 make g++ ffmpeg git

# Copy workspace files
COPY pnpm-lock.yaml pnpm-workspace.yaml package.json ./
COPY api/package.json ./api/
COPY packages/version-info/ ./packages/version-info/

# Install dependencies (don't use --prod because youtubei.js from git needs to build)
RUN pnpm install --frozen-lockfile

# Copy API source code
COPY api/src ./api/src

# Copy .git for version-info package (needs git info)
COPY .git ./.git

WORKDIR /app/api

# Expose port
EXPOSE 9111

# Start the application
CMD ["pnpm", "start"]
