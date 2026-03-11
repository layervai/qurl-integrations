# layerv-qurl

Python SDK for the [QURL API](https://docs.layerv.ai) — secure, time-limited access links for AI agents.

## Installation

```bash
pip install layerv-qurl
```

For LangChain integration:

```bash
pip install layerv-qurl[langchain]
```

## Quick Start

```python
from layerv_qurl import QURLClient, CreateInput, ResolveInput

client = QURLClient(api_key="lv_live_xxx")

# Create a protected link
result = client.create(CreateInput(
    target_url="https://api.example.com/data",
    expires_in="24h",
    description="API access for agent"
))
print(result.qurl_link)

# Resolve a token (opens firewall for your IP)
access = client.resolve(ResolveInput(access_token="at_..."))
print(f"Access granted to {access.target_url} for {access.access_grant.expires_in}s")
```

## LangChain Integration

```python
from layerv_qurl import QURLClient
from layerv_qurl.langchain import QURLToolkit

client = QURLClient(api_key="lv_live_xxx")
toolkit = QURLToolkit(client=client)
tools = toolkit.get_tools()  # [CreateQURLTool, ResolveQURLTool, ListQURLsTool, DeleteQURLTool]
```

## Configuration

| Parameter | Required | Default |
|-----------|----------|---------|
| `api_key` | Yes | — |
| `base_url` | No | `https://api.layerv.ai` |
| `timeout` | No | `30.0` |
| `max_retries` | No | `3` |

## License

MIT
