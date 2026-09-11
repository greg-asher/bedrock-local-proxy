# Bedrock Local Proxy

Bedrock Local Proxy is a localhost-only Go executable that exposes Amazon Bedrock through the request formats used by OpenAI-compatible clients and Claude Code. It signs upstream requests with the AWS profile configured on the machine. Prompts, responses, tool content, credentials, and sensitive headers are not written to the proxy logs.

## Requirements

- Go (the current supported Go version used by the repository)
- AWS CLI v2 with access to your organization’s IAM Identity Center (AWS SSO) account and role
- A Bedrock model or inference profile that the selected AWS role can invoke

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

models:
  coding:
    display_name: Claude Sonnet for coding
    bedrock_model_id: REPLACE_WITH_YOUR_BEDROCK_MODEL_ID
    input_per_million: 0
    output_per_million: 0
    temperature: 0.2
    max_tokens: 8192
```

Replace `YOUR_AWS_PROFILE` with the named AWS CLI profile you create or already use, and replace `REPLACE_WITH_YOUR_BEDROCK_MODEL_ID` with an accessible Bedrock model or inference-profile ID. The model name under `models` is the local alias clients send. `bedrock_model_id` is the actual Bedrock model or inference-profile ID sent upstream. Add one entry per target you want clients to select, for example `coding` and `fast`. The proxy lists these aliases from `GET /v1/models`.

Configuration fields:

| Field | Required | Meaning |
| --- | --- | --- |
| `version` | yes | Configuration format version. Use `1`. |
| `aws.profile` | yes | Named AWS CLI profile used for SSO credentials. |
| `aws.region` | yes | Bedrock runtime region, such as `us-east-2`. |
| `listen` | no | Loopback TCP address. The default is `127.0.0.1:8787`; non-loopback addresses are rejected. |
| `models.<alias>.display_name` | no | Human-readable label for the configured target; the alias remains the client-facing model name. |
| `models.<alias>.bedrock_model_id` | yes | Bedrock model ID or inference-profile ID. |
| `models.<alias>.input_per_million` | no | Input price in dollars per million tokens, used only for estimated logs. `0` is valid. |
| `models.<alias>.output_per_million` | no | Output price in dollars per million tokens, used only for estimated logs. `0` is valid. |
| `models.<alias>.temperature` | no | Default temperature inserted only when the request omits it. |
| `models.<alias>.max_tokens` | no | Default output-token limit inserted only when the request omits it. |

Prices are never fetched automatically. If the upstream response does not contain both token counts, or either price is omitted, the log reports the estimate as unavailable rather than treating it as zero. Keep model aliases free of surrounding whitespace and use finite, nonnegative prices.

The process accepts these command-line options:

```text
--config PATH       load a different YAML file
--log-format text   human-readable startup, request, and summary logs (default)
--log-format json   one JSON object per log line
--version           print the installed version and exit
```

For example:

```sh
bedrock-proxy --config ./config.yaml --log-format json
```

There are no configuration environment variables. Use `--config` when a separate file is needed.

## 4. Start the proxy

After editing the model target and logging in with SSO, start it with:

```sh
aws sso login --profile YOUR_AWS_PROFILE
bedrock-proxy
```

Successful startup prints the local URL, AWS profile, Bedrock region, and configured aliases. Startup does not contact AWS; the first generation request loads credentials, signs the request, and proves model access. Stop the proxy with `Ctrl-C`. It stops admitting new requests and drains active requests for up to five seconds.

The local API is available at:

```text
http://127.0.0.1:8787/v1
```

Use `local` as a nonempty dummy API key in clients. It is only for satisfying client-side configuration; the proxy removes local authorization and API-key headers before signing the AWS request.

## 5. Point a client at the local API

### OpenAI-compatible clients

Set the client’s base URL to `http://127.0.0.1:8787/v1`, use `local` as the API key, and select a configured alias such as `coding`.

Check the aliases:

```sh
curl http://127.0.0.1:8787/v1/models \
  -H 'Authorization: Bearer local'
```

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

## Cross-build checks

The normal install builds only for the current host. To check all supported targets without installing them, run:

```sh
./scripts/check-build-targets.sh
```

This checks compilation for `darwin/arm64`, `darwin/amd64`, `linux/amd64`, and `linux/arm64`. The generated binaries are not run during cross-build checks because their runtime platform may differ from the development machine.
