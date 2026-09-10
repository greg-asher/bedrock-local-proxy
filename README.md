# Bedrock Local Proxy

Bedrock Local Proxy exposes a local OpenAI-compatible endpoint backed by Amazon Bedrock. The proxy uses the AWS profile already configured on the developer’s machine and keeps prompts and responses out of logs.

## First local startup

Install Go, then build the current checkout:

```sh
./install.sh
```

The install script is added by issue 11. Until it lands, build and run the command directly:

```sh
go build -o "$HOME/.local/bin/bedrock-proxy" ./cmd/bedrock-proxy
```

The default configuration file is:

```text
~/.config/bedrock-proxy/config.yaml
```

Copy `config.example.yaml` there and replace `REPLACE_WITH_YOUR_BEDROCK_MODEL_ID` with a model or inference-profile ID that your AWS account can use. Keep the profile as `Halo-Win-Agent-Execution` and the region as `us-east-2` unless your local configuration intentionally uses another approved profile or region.

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
