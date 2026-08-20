# [AGENTS.md](http://AGENTS.md)

## Environment

- You are running in an isolated Debian  Linux VM.

## Dev Containers

- Always perform development inside a VS Code Dev Container.
- Use the installed Dev Containers CLI for all Dev Container operations. Check the current upstream Dev Containers CLI documentation before use.
- Start or rebuild a development container with `devcontainer up --workspace-folder <path>` and run commands with `devcontainer exec --workspace-folder <path> <command>`.
- If the repository does not already contain a `.devcontainer/` configuration, create one before beginning development.
- Reuse and update an existing `.devcontainer/` configuration when one is present instead of creating an alternative development environment.
- Configure Dev Containers with the project's required tools, runtimes, dependencies, and extensions so development, builds, and tests run inside the container.
- Do not install project-specific development dependencies directly on the agent-os guest when they can be installed in the Dev Container.
- Use Docker as the container runtime for Dev Container-related container operations.

## Containers

- Always use multi-stage container builds for application images.
- Use dedicated build stages for compilation, dependency installation, code generation, tests, and other build-time tooling.
- Keep compilers, package managers, shells, source files, caches, and other build-only dependencies out of final runtime images.
- Final CI runtime and production images must use an appropriate Google Distroless base image whenever the application requires a runtime image.
- Prefer the corresponding `nonroot` Distroless image variant when one is available and compatible with the application.
- Run application processes as a non-root user.
- Copy only the artifacts and runtime dependencies required to execute the application into the final image.
- Do not install packages or debugging utilities into Distroless runtime stages.
- Do not assume a shell is available in Distroless images. Use exec-form `ENTRYPOINT` and `CMD` instructions.
- Keep development tooling in the VS Code Dev Container and build stages rather than in CI or production runtime images.
- Use the same final runtime image architecture and configuration in CI validation that will be deployed to production whenever practical.
- If a project cannot use a Distroless runtime image because of a concrete technical requirement, document the reason in the repository and use the smallest compatible runtime image instead.

## Kubernetes

- A ready single-node k3s cluster with Cilium as its CNI is available locally. Use `kubectl` and the default kubeconfig at `$HOME/.kube/config` for Kubernetes work.
- When a Kubernetes-related task warrants a fresh local cluster, create it with `sudo agent-os-k3s create` or recreate it with `sudo agent-os-k3s reset`.
- `reset` and `delete` permanently remove local cluster workloads and state. Use them only when the task requires it and losing the existing cluster state is acceptable.
- Delete the local cluster with `sudo agent-os-k3s delete` only when the task explicitly warrants removing it.

## Packages and Applications

- If a package or application is added to agent-os provisioning, implement and test its installation on both Fedora and Debian, including architecture-specific handling where required.
- Put applications that use the same installer on both distributions in shared provisioning. Keep only package-manager and container-runtime differences in distro-specific provisioning.
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

## Browser tooling

Two browser CLIs are available. Choose the tool based on the purpose of the task.

### Use Playwright CLI for browser interaction and functional verification

Use `playwright-cli` when the primary goal is to **operate the application as a user would** or verify application behavior.

Typical cases:

- Navigate through application flows.
- Click, type, select, drag, upload files, or otherwise interact with UI controls.
- Verify that a feature works end-to-end.
- Reproduce a user-facing bug through a sequence of actions.
- Inspect page structure or accessibility snapshots to locate elements.
- Test forms, authentication flows, navigation, dialogs, downloads, and multi-tab behavior.
- Capture screenshots as evidence of functional or visual behavior.
- Test different browsers, devices, viewports, or browser contexts.
- Create or validate Playwright locators and Playwright tests.
- Mock or intercept requests when doing so is part of a functional test.

**Default rule:** if the task can be expressed as "open the application, perform these user actions, and verify the result," use Playwright CLI.

### Use Chrome DevTools CLI for diagnosis and browser-level investigation

Use `chrome-devtools` when the primary goal is to **understand why something is happening inside Chrome**, rather than simply exercise a user flow.

Typical cases:

- Investigate frontend performance problems.
- Record and analyze Chrome performance traces.
- Diagnose Core Web Vitals such as LCP, INP, and CLS.
- Run Lighthouse audits.
- Inspect individual network requests, responses, headers, timing, or failed requests in detail.
- Investigate console errors or warnings while debugging.
- Inspect JavaScript runtime behavior when diagnosing a problem.
- Investigate memory usage, heap snapshots, retained objects, leaks, or allocation problems.
- Apply CPU or network throttling for diagnostic purposes.
- Debug Chrome-specific behavior or behavior that requires Chrome DevTools instrumentation.

**Default rule:** if the task can be expressed as "determine why the browser/application behaves this way," use Chrome DevTools CLI.

### When both are useful

Use the tools sequentially when a task has separate reproduction and diagnosis phases:

1. Use Playwright CLI to reproduce the user-visible problem reliably.
2. Use Chrome DevTools CLI to investigate its cause through performance, network, console, runtime, or memory diagnostics.
3. After making a fix, use Playwright CLI again to verify the user-visible behavior.

Do not use both tools merely because both can perform a particular browser action. Prefer the tool whose primary role matches the current task.

### Selection shorthand

- **Interact / exercise / verify / test a flow → Playwright CLI**
- **Inspect / diagnose / profile / analyze browser internals → Chrome DevTools CLI**
