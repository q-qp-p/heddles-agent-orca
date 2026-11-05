# Sandbox image for running the Claude Code CLI in an agent-orca sandbox.
# See docs/claude-code-subscription.md.
FROM node:22-bookworm-slim
RUN npm install -g @anthropic-ai/claude-code && claude --version
