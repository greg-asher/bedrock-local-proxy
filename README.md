# Bedrock Local Proxy

Bedrock Local Proxy is a localhost-only Go executable that exposes Amazon Bedrock through the request formats used by OpenAI-compatible clients and Claude Code. It signs upstream requests with the AWS profile configured on the machine. Prompts, responses, tool content, credentials, and sensitive headers are not written to the proxy logs.

## Requirements

- Go (the current supported Go version used by the repository)
- AWS CLI v2 with access to your organization’s IAM Identity Center (AWS SSO) account and role
- A Bedrock model or inference profile that the selected AWS role can invoke
- Codex CLI when generating or validating a Codex model catalog

## 1. Configure AWS SSO

The proxy uses the AWS SDK credential chain. It does not ask for access keys and it does not perform the browser login itself. Configure a named AWS CLI profile once, then refresh its SSO session when needed.

If the profile has not been configured on this machine, run:

```sh
aws configure sso --profile YOUR_AWS_PROFILE
```

Follow the prompts in the terminal and browser. Use the IAM Identity Center start URL supplied by your administrator, the SSO region that hosts your organization’s Identity Center directory, the AWS account and permission set that may invoke Bedrock, and `YOUR_AWS_PROFILE` as the profile name. The SSO region is the directory’s region; it can be different from the Bedrock runtime region in the proxy configuration. See the [AWS CLI IAM Identity Center guide](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-sso.html) if your profile is not yet provisioned.

Start or refresh the browser-backed session before running the proxy:

```sh
aws sso login --profile YOUR_AWS_PROFILE
aws sts get-caller-identity --profile YOUR_AWS_PROFILE
```

The second command should return the account and role you intend to use. The AWS CLI caches the temporary SSO session locally; do not copy access keys into the proxy configuration.

## 2. Build and install

From a checkout of this repository, run:

```sh
./install.sh
```

The script builds for the current host, installs the binary at `~/.local/bin/bedrock-proxy`, and creates the configuration at:

```text
~/.config/bedrock-proxy/config.yaml
```

It creates the configuration only when it is missing. Re-running the script updates the binary and preserves the existing configuration byte-for-byte. If `~/.local/bin` is not already on `PATH`, add it for the current shell:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

## 3. Configure the proxy

Edit `~/.config/bedrock-proxy/config.yaml`:

```yaml
version: 1

aws:
  profile: YOUR_AWS_PROFILE
  region: us-east-2

listen: 127.0.0.1:8787

# Optional. Relative paths resolve from this configuration file.
# reporting:
#   directory: reports

models:
  coding:
    display_name: Responses-compatible coding model
    bedrock_model_id: REPLACE_WITH_YOUR_BEDROCK_MODEL_ID
    input_per_million: 0
    output_per_million: 0
    temperature: 0.2
    max_tokens: 8192

    # Required for Codex. These are examples; use documented values for the
    # exact configured target or an exact bundled metadata_profile.
    capabilities:
      responses_api: true
      context_window: 200000
      max_output_tokens: 64000
      input_modalities: [text, image]
      reasoning:
        supported: true
        efforts: [low, medium, high]
      tools:
        function_calling: true
        parallel_calls: true
```

Replace `YOUR_AWS_PROFILE` with the named AWS CLI profile you create or already use, and replace `REPLACE_WITH_YOUR_BEDROCK_MODEL_ID` with an accessible Bedrock model or inference-profile ID. The model name under `models` is the local alias clients send. `bedrock_model_id` is the actual Bedrock model or inference-profile ID sent upstream. Add one entry per target you want clients to select, for example `coding` and `fast`. The proxy lists these aliases from `GET /v1/models`.

Configuration fields:

| Field | Required | Meaning |
| --- | --- | --- |
| `version` | yes | Configuration format version. Use `1`. |
| `aws.profile` | yes | Named AWS CLI profile used for SSO credentials. |
| `aws.region` | yes | Bedrock runtime region, such as `us-east-2`. |
| `listen` | no | Loopback TCP address. The default is `127.0.0.1:8787`; non-loopback addresses are rejected. |
| `reporting.directory` | no | Parent directory for private per-session reports. A relative path resolves from the configuration file. |
| `models.<alias>.display_name` | no | Human-readable label for the configured target; the alias remains the client-facing model name. |
| `models.<alias>.bedrock_model_id` | yes | Bedrock model ID or inference-profile ID. |
| `models.<alias>.input_per_million` | no | Input price in dollars per million tokens, used only for estimated logs. `0` is valid. |
| `models.<alias>.output_per_million` | no | Output price in dollars per million tokens, used only for estimated logs. `0` is valid. |
| `models.<alias>.temperature` | no | Default temperature inserted only when the request omits it. |
| `models.<alias>.max_tokens` | no | Default output-token limit inserted only when the request omits it. |
| `models.<alias>.capabilities.metadata_profile` | Codex only | Exact bundled metadata profile. Profiles are matched only by name or exact Bedrock target ID. |
| `models.<alias>.capabilities.responses_api` | Codex only | Whether the exact target supports Responses on `bedrock-runtime`. Required for unrecognized targets; set it only from authoritative AWS compatibility documentation. |
| `models.<alias>.capabilities.context_window` | Codex only | Documented hard total-context limit. Explicit values override a selected profile. |
| `models.<alias>.capabilities.max_output_tokens` | Codex only | Documented hard output limit. `max_tokens` must not exceed it. |
| `models.<alias>.capabilities.input_modalities` | Codex only | Supported input types: `text` and optionally `image`. |
| `models.<alias>.capabilities.reasoning` | Codex only | Whether adjustable reasoning is supported and the exact supported efforts. |
| `models.<alias>.capabilities.tools` | Codex only | Client-side function-calling and parallel-call support. |

Prices are never fetched automatically. If the upstream response does not contain both token counts, or either price is omitted, the log reports the estimate as unavailable rather than treating it as zero. Keep model aliases free of surrounding whitespace and use finite, nonnegative prices.

Capability numbers in the example are placeholders, not proxy defaults. `configure codex` requires complete resolved metadata and confirmed Responses support, then fails with the missing fields rather than guessing. Exact bundled profiles currently cover [`openai.gpt-oss-120b-1:0`](https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-openai-gpt-oss-120b.html), [`openai.gpt-oss-20b-1:0`](https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-openai-gpt-oss-20b.html), and their documented GovCloud inference IDs. Their AWS model cards document a 128,000-token context window, a 16,000-token output ceiling, text input, and Responses support; AWS also documents [client-side function calling through Responses](https://docs.aws.amazon.com/bedrock/latest/userguide/tool-use-client-side.html). The [Sonnet 4.5 model card](https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-anthropic-claude-sonnet-4-5.html) is retained only to produce a precise incompatibility error because AWS documents that target as unsupported by Responses. Explicit values take precedence over a compatible profile for model limits, but cannot turn on an API that AWS documents as unsupported. Existing version `1` configurations without `capabilities` continue to serve other clients.

Codex CLI `0.142.5` exposes context, modality, reasoning, parallel-tool, and search capabilities through its model catalog, but it does not expose a model output-ceiling field. The generated catalog retains `max_output_tokens` in proxy-owned metadata, and the proxy enforces that hard ceiling on every Responses request. When Codex omits a request limit, `models.<alias>.max_tokens` supplies the request default. `configure codex` and `doctor` report this client limitation explicitly so an output ceiling is never mistaken for metadata Codex consumed.

The process accepts these command-line options:

```text
--config PATH       load a different YAML file
--log-format text   human-readable startup, request, and summary logs (default)
--log-format json   one JSON object per log line
--report-dir PATH   parent directory for one run's session report
--session-tag NAME  optional ingestion tag for one run
--version           print the installed version and exit
```

For example:

```sh
bedrock-proxy --config ./config.yaml --log-format json --session-tag nightly-ingest
```

There are no configuration environment variables. `XDG_STATE_HOME` only selects the default report parent; use `--config` when a separate configuration file is needed.

## 4. Start the proxy

After editing the model target and logging in with SSO, start it with:

```sh
aws sso login --profile YOUR_AWS_PROFILE
bedrock-proxy
```

Successful startup prints the local URL, AWS profile, Bedrock region, configured aliases, and the absolute session-report path. `--session-tag` also prints the supplied tag. Startup does not contact AWS; the first generation request loads credentials, signs the request, and proves model access. Stop the proxy with `Ctrl-C`. It stops admitting new requests and drains active requests for up to five seconds.

Every run writes a private report by default. The parent directory is `$XDG_STATE_HOME/bedrock-proxy/sessions` when `XDG_STATE_HOME` is an absolute path, otherwise `~/.local/state/bedrock-proxy/sessions`. Use `reporting.directory` for a persistent override or `--report-dir` for one run; relative CLI paths resolve from the current directory.

Each report contains `session.json`, `events.jsonl`, and `summary.json`. The report retains request outcome, endpoint, alias, configured Bedrock target ID, latency, status, observed token counts, and estimated cost. It does not retain the AWS profile, prompts, completions, tool content, request bodies, headers, credentials, or raw upstream errors. Report directories use owner-only permissions and recognized reports older than 30 days are removed when the proxy starts. Cleanup warnings do not stop the proxy.

## Create a period report

Generate a standalone HTML report from locally stored session events. It includes request and cost charts, token and estimated-cost totals, success and failure coverage, function-call and unsupported-feature counts, utilization metrics, and model, endpoint, tag, client, metadata-profile, and catalog breakdowns. It does not contact AWS or need credentials.

```sh
bedrock-proxy report \
  --start 2026-09-01T00:00:00Z \
  --stop 2026-09-08T00:00:00Z
```

`--start` is inclusive and `--stop` is exclusive, so adjacent reports do not double-count requests. Both accept RFC3339 timestamps or `YYYY-MM-DD`. The default report is an owner-only HTML file under `<report-parent>/summaries`; the command prints its absolute path. Use `--report-dir` to read a different report parent, `--config` to use a configuration file’s `reporting.directory`, `--output PATH` to choose the output file, or `--format json` for the structured aggregate without charts.

The local API is available at:

```text
http://127.0.0.1:8787/v1
```

Use `local` as a nonempty dummy API key in clients. It is only for satisfying client-side configuration; the proxy removes local authorization and API-key headers before signing the AWS request.

## 5. Point a client at the local API

### Codex CLI

Generate a catalog after setting accurate capabilities for the selected alias:

```sh
bedrock-proxy configure codex --model coding
```

With one configured alias, `--model` is optional. With several aliases it is required. The command reads the installed Codex bundled catalog, preserves its entries, adds the selected local alias, verifies that Codex can parse the result, and writes it with owner-only permissions. The default path is `~/.config/bedrock-proxy/clients/codex/models.json`; a custom proxy configuration places it beside that configuration under `clients/codex`, and `--catalog` overrides it.

Choose a local alias that does not match a Codex bundled model slug. Catalog generation rejects collisions so it cannot replace built-in Codex metadata.

The command prints the complete `bedrock-local` provider/profile TOML and the profile file path. Add that TOML at the printed path. It uses `http://127.0.0.1:8787/v1`, the Responses wire API, a non-secret local bearer value, the generated catalog, and the selected alias. It sets `web_search = "disabled"` and `tools.web_search = false` so Codex does not offer OpenAI-hosted search while MCP tools remain available. It also disables ChatGPT apps in this local-provider profile because their internal `codex_apps` MCP may require a desktop connection that is unavailable on the proxy host. Explicitly configured MCP servers, including search MCPs, remain enabled. It does not modify Codex configuration. Regenerate after changing the selected target, display name, or any capability; `doctor` compares the active configuration fingerprint with the catalog and rejects stale metadata.

Run the offline compatibility checks before AWS login:

```sh
bedrock-proxy doctor --client codex --model coding
```

After the proxy is running and SSO is active, exercise model discovery, nonstreaming and streaming Responses, cancellation, usage, and a function-call/result continuation:

```sh
bedrock-proxy doctor --client codex --model coding --live
codex --profile bedrock-local
```

OpenAI `web_search` is a server-hosted Responses tool. The [Bedrock Runtime Responses endpoint](https://docs.aws.amazon.com/bedrock/latest/userguide/bedrock-mantle.html) supports client-side tools but does not execute that hosted tool, so the generated catalog advertises hosted search as unavailable. Configure the desired search provider as an MCP in Codex instead. Codex discovers and executes the MCP, while the proxy transports resulting function calls and outputs without inspecting their content. Disabling hosted search or ChatGPT apps does not disable configured MCP tools. If a client still sends a hosted tool, the proxy returns `400 unsupported_hosted_tool` before making an AWS request. The generated keys follow the [Codex configuration reference](https://developers.openai.com/codex/config-reference).

The complete clean-install sequence is:

```sh
./install.sh
# edit ~/.config/bedrock-proxy/config.yaml
bedrock-proxy configure codex --model coding
# add the printed provider/profile TOML at the printed Codex profile path
# configure the desired search MCP in Codex
bedrock-proxy doctor --client codex --model coding
aws sso login --profile YOUR_AWS_PROFILE
bedrock-proxy --session-tag NAME
bedrock-proxy doctor --client codex --model coding --live
codex --profile bedrock-local
```

### OpenAI-compatible clients

Set the client’s base URL to `http://127.0.0.1:8787/v1`, use `local` as the API key, and select a configured alias such as `coding`.

Check the aliases:

```sh
curl http://127.0.0.1:8787/v1/models \
  -H 'Authorization: Bearer local'
```

Retrieve one alias with `GET /v1/models/coding`. Both discovery routes return standard model identity fields; Codex-specific capability metadata lives in the generated catalog.

Send a Chat Completions request:

```sh
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Authorization: Bearer local' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "coding",
    "messages": [{"role": "user", "content": "Say hello in one sentence."}],
    "stream": true
  }'
```

The proxy also exposes OpenAI Responses at `/v1/responses`. Both OpenAI routes preserve request fields and rewrite only the local model alias to the configured Bedrock target.

### Claude Code and Anthropic Messages

The native Anthropic Messages route is `/v1/messages`. Claude Code’s gateway configuration uses the proxy root as its base URL, so set the endpoint without the `/v1` suffix:

```sh
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_API_KEY=local
export ANTHROPIC_MODEL=coding
claude
```

`ANTHROPIC_MODEL` must match a model alias in the YAML file. Claude Code may discover aliases through `GET /v1/models` when gateway model discovery is enabled in the installed version; otherwise select the alias explicitly in Claude Code. See the [Claude Code gateway configuration](https://code.claude.com/docs/en/llm-gateway) and [environment variable reference](https://code.claude.com/docs/en/env-vars) for client-version-specific settings.

You can exercise the same route directly:

```sh
curl http://127.0.0.1:8787/v1/messages \
  -H 'x-api-key: local' \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "coding",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Say hello in one sentence."}],
    "stream": true
  }'
```

The proxy forwards Anthropic headers and the Messages request shape to Bedrock’s Anthropic-compatible route, including streaming and tool-use fields. It signs the upstream request with the configured AWS profile.

## Troubleshooting

- **Configuration file not found:** run `./install.sh` or pass the correct path with `--config`.
- **AWS credentials are unavailable:** run `aws sso login --profile <the profile in config.yaml>` and retry.
- **Credentials have expired:** run the same `aws sso login` command again; no access keys need to be exported.
- **Unknown model:** use an alias defined under `models`, then verify it with `curl http://127.0.0.1:8787/v1/models`.
- **Port already in use:** change `listen` to another loopback port and update the client base URL.
- **Model access denied or unsupported:** confirm that the selected role, region, and Bedrock model/inference profile are compatible.
- **Codex reports fallback or missing model metadata:** rerun `bedrock-proxy configure codex --model <alias>`, replace the profile TOML, then run the offline doctor. Regenerate after a Codex upgrade when doctor reports catalog drift.
- **Codex reports `codex_apps` was not initialized:** regenerate and replace the Bedrock profile. The generated profile disables ChatGPT apps for this local provider while leaving configured MCP servers available.
- **Codex target does not support Responses:** select a target whose exact AWS model card lists Responses support on `bedrock-runtime`. `configure codex`, `doctor`, and known-incompatible live requests fail locally before AWS transport.
- **Codex asks for hosted web search:** configure search as an MCP in Codex. The proxy deliberately rejects hosted Responses tools because Bedrock Runtime does not execute them.

## Cross-build checks

The normal install builds only for the current host. To check all supported targets without installing them, run:

```sh
./scripts/check-build-targets.sh
```

This checks compilation for `darwin/arm64`, `darwin/amd64`, `linux/amd64`, and `linux/arm64`. The generated binaries are not run during cross-build checks because their runtime platform may differ from the development machine.
