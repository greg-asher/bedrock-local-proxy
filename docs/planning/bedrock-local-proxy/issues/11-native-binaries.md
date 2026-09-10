# Build and install locally with one command

## Useful outcome

A developer with Go installed runs `./install.sh` from the checkout and gets a usable local command plus a starter configuration.

## What changes

Provide a small repository-root `install.sh` that builds the current source for the host machine and installs `bedrock-proxy` into `~/.local/bin`. Check the Go prerequisite and report build failures clearly. Build successfully before replacing an existing binary. Run the installed binary with `--version` to verify installation. Do not require sudo.

Create `~/.config/bedrock-proxy/config.yaml` from the example owned by issue 01 only when the file does not already exist. Include the actual AWS profile and region and an obvious model-ID placeholder for the developer to replace. Preserve existing configuration on every subsequent install. Do not start the proxy with a placeholder model or claim AWS connectivity has been checked.

Finish with a short next-steps message: edit the displayed config path to supply a model ID, show `aws sso login --profile Halo-Win-Agent-Execution` for the newly generated example, or tell users with existing configuration to use its profile name, then run `bedrock-proxy`. If `~/.local/bin` is absent from PATH, show the shell command to add it; do not edit shell startup files. Keep the installed absolute binary path usable immediately.

Document the first-use sequence as checkout, `./install.sh`, edit configuration, SSO login, start proxy, and configure the client. Re-running the script is the update workflow. Build the current host only during installation; retain simple cross-build checks for the four supported targets without making the developer build all four.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), sections 16–18, amended by the user’s clarification that developers build on their own machines. No downloadable release, remote installer, checksum download flow, publication, package manager, or release infrastructure is needed.

Supported targets remain darwin/arm64, darwin/amd64, linux/amd64, and linux/arm64. Go is a build prerequisite; the resulting executable needs no Go installation to run. AWS CLI/profile setup and client installation remain prerequisites for using the product, not tasks performed by this script. Never change AWS credentials or trigger interactive login during installation.

Issue 01 owns the executable, version output, and example configuration. This issue owns installation and its concise instructions. Installation is independent of live AWS acceptance. Keep the script small: no interactive wizard, background service, automatic shell edits, or additional configuration system.

## Done when

- On an available supported host, one invocation builds and installs the command, verifies its version, and creates starter configuration only if absent.
- A second invocation updates the binary while preserving existing configuration byte-for-byte.
- Missing Go or a failed build exits nonzero with a useful message and leaves the existing installation intact.
- Paths containing spaces work; installation uses no sudo and modifies no AWS or shell configuration.
- A missing PATH entry yields a usable instruction and the installed absolute path; successful installation is distinguished from successful AWS access.
- Simple cross-build checks cover all four targets; unavailable runtime checks are labeled. The normal install builds only for the host.
- The documented journey uses this script, and final acceptance runs against a binary installed from the accepted source.

## Depends on

- [01: Start locally and list configured models](01-local-startup.md)
