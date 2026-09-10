# Bedrock Local Proxy

Bedrock Local Proxy exposes a local OpenAI-compatible endpoint backed by Amazon Bedrock. The proxy uses the AWS profile already configured on the developer’s machine and keeps prompts and responses out of logs.

## First local startup

Install Go, then build and install the current checkout:

```sh
./install.sh
```

The default configuration file is:

```text
~/.config/bedrock-proxy/config.yaml
```

`./install.sh` creates this file from `config.example.yaml` when it is missing. Replace `REPLACE_WITH_YOUR_BEDROCK_MODEL_ID` with a model or inference-profile ID that your AWS account can use. Re-running `./install.sh` updates the binary and preserves the existing configuration byte-for-byte. Keep the profile as `Halo-Win-Agent-Execution` and the region as `us-east-2` unless your local configuration intentionally uses another approved profile or region.

Sign in through the existing AWS CLI profile, then start the proxy:

```sh
aws sso login --profile Halo-Win-Agent-Execution
bedrock-proxy
```

The proxy listens at `http://127.0.0.1:8787/v1`. Set an OpenAI-compatible client’s base URL to that address, use `local` as its nonempty API key, and select one of the configured model names such as `coding`.

Use another configuration file when needed:

```sh
bedrock-proxy --config ./config.yaml
```

Startup proves that the local server is listening and the configuration is valid. The first successful request proves AWS credentials, model access, signing, and the upstream endpoint. The proxy does not automate interactive SSO login and does not store AWS credentials.

## Cross-build checks

The normal install builds only for the current host. To check all supported targets without installing them, run:

```sh
./scripts/check-build-targets.sh
```

This checks compilation for `darwin/arm64`, `darwin/amd64`, `linux/amd64`, and `linux/arm64`. The generated binaries are not run during cross-build checks because their runtime platform may differ from the development machine.
