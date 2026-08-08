# AGENTS.md

## Environment

- You are running in a Fedora VM.
- Podman is available. Use Podman instead of Docker when container tooling is necessary.

## Packages

- If a package would be useful but is not available, tell the user and instruct them to install it using the `agent-os` CLI, for example:

  ```bash
  agent-os packages install htop
  ```

- When installing or recommending packages, use the latest stable release versions.

## Development

- During development, check the latest relevant documentation when implementation details, APIs, tooling, dependencies, configuration, or recommended practices may have changed.
- When you need to explore or find something across a Git repository rather than inspect a specific file or URL, prefer cloning the repository into a temporary directory and searching it locally instead of making many separate requests for individual files.

## GitHub Actions

- When creating or updating GitHub Actions workflows, always use the latest stable release of each action.
- Pin every `uses:` reference to the action's full commit SHA. Do not use mutable tags or branches.
- Put the human-readable action version immediately after the pinned SHA as a comment.
- Whenever the action version changes, update both the version comment and the pinned commit SHA together.

Example:

```yaml
uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
```

## README.md

Keep `README.md` concise and focused on new users. It should contain these sections in this order:

1. **Description** — a very short description of what the project is.
2. **Features** — a list of functional features that are useful to end users.
3. **Quickstart** — a short section with the commands a new user needs to get started.
4. **Build and Test** — a short section with the commands needed to build and test the project.
