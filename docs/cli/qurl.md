## qurl

qURL CLI - manage secure links from the command line

### Synopsis

qURL CLI creates, resolves, and manages qURL secure links.

Authentication (in order of precedence):
  1. --api-key flag (visible in process list — prefer env var)
  2. QURL_API_KEY environment variable (recommended)
  3. ~/.config/qurl/config.yaml (or --profile NAME)

Get started:
  qurl create https://example.com        Create a qURL
  qurl list                              List active qURLs
  qurl resolve ACCESS_TOKEN              Resolve a token (headless)
  qurl quota                             Check your usage
  qurl completion bash                   Generate shell completions

### Options

```
      --api-key string    API key (prefer env var or config file to avoid exposure in process list)
      --endpoint string   API endpoint (default: https://api.layerv.ai)
  -h, --help              help for qurl
  -o, --output string     Output format: table or json (default "table")
      --profile string    Config profile name (reads ~/.config/qurl/profiles/NAME.yaml)
  -q, --quiet             Minimal output (just the essential value)
  -v, --verbose           Show HTTP request/response details
```

### SEE ALSO

* [qurl completion](qurl_completion.md)	 - Generate shell completion scripts
* [qurl config](qurl_config.md)	 - Manage CLI configuration
* [qurl create](qurl_create.md)	 - Create a qURL for a target URL
* [qurl delete](qurl_delete.md)	 - Revoke/delete a qURL
* [qurl extend](qurl_extend.md)	 - Extend qURL expiration
* [qurl get](qurl_get.md)	 - Get qURL details
* [qurl list](qurl_list.md)	 - List qURLs
* [qurl mint](qurl_mint.md)	 - Mint a new access link for a qURL
* [qurl quota](qurl_quota.md)	 - Show usage quota and plan info
* [qurl resolve](qurl_resolve.md)	 - Resolve a qURL access token (headless)
* [qurl update](qurl_update.md)	 - Update a qURL's properties
* [qurl version](qurl_version.md)	 - Print version information
