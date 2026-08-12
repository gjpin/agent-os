# AGENTS.md

## Environment

* You are running in a Fedora VM.
* Podman is available. Use Podman instead of Docker when container tooling is necessary.
* Always perform development inside a VS Code Dev Container.
* If the repository does not already contain a `.devcontainer/` configuration, create one before beginning development.
* Reuse and update an existing `.devcontainer/` configuration when one is present instead of creating an alternative development environment.
* Configure Dev Containers with the project's required tools, runtimes, dependencies, and extensions so development, builds, and tests run inside the container.
* Do not install project-specific development dependencies directly on the Fedora host when they can be installed in the Dev Container.
* Use Podman as the container runtime for Dev Container-related container operations.

## Containers

* Always use multi-stage container builds for application images.
* Use dedicated build stages for compilation, dependency installation, code generation, tests, and other build-time tooling.
* Keep compilers, package managers, shells, source files, caches, and other build-only dependencies out of final runtime images.
* Final CI runtime and production images must use an appropriate Google Distroless base image whenever the application requires a runtime image.
* Prefer the corresponding `nonroot` Distroless image variant when one is available and compatible with the application.
* Run application processes as a non-root user.
* Copy only the artifacts and runtime dependencies required to execute the application into the final image.
* Do not install packages or debugging utilities into Distroless runtime stages.
* Do not assume a shell is available in Distroless images. Use exec-form `ENTRYPOINT` and `CMD` instructions.
* Keep development tooling in the VS Code Dev Container and build stages rather than in CI or production runtime images.
* Use the same final runtime image architecture and configuration in CI validation that will be deployed to production whenever practical.
* If a project cannot use a Distroless runtime image because of a concrete technical requirement, document the reason in the repository and use the smallest compatible runtime image instead.

## Packages

* If a package would be useful but is not available, tell the user and instruct them to install it using the `agent-os` CLI, for example:

  ```bash
  agent-os packages install htop
  ```

* When installing or recommending packages, use the latest stable release versions.

## Development

* During development, check the latest relevant documentation when implementation details, APIs, tooling, dependencies, configuration, or recommended practices may have changed.
* When you need to explore or find something across a Git repository rather than inspect a specific file or URL, prefer cloning the repository into a temporary directory and searching it locally instead of making many separate requests for individual files.

## Chrome DevTools MCP

* Chrome DevTools MCP and its CLI are installed in the VM for browser debugging. Chromium is supported on a best-effort basis; launch it with:

  ```sh
  chrome-devtools start --headless --executable-path="$(command -v chromium)"
  ```

* Use `agent-os skills install [name]` to apply the configured package and skills to an existing VM.

## GitHub Actions

* When creating or updating GitHub Actions workflows, always use the latest stable release of each action.
* Pin every `uses:` reference to the action's full commit SHA. Do not use mutable tags or branches.
* Put the human-readable action version immediately after the pinned SHA as a comment.
* Whenever the action version changes, update both the version comment and the pinned commit SHA together.

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
