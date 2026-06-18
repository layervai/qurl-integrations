## qurl mint

Mint a new access link for a qURL

### Synopsis

Creates a new access token and link for an existing qURL resource.
Useful for multi-use qURLs where you want to generate additional access links.

```
qurl mint RESOURCE_ID [flags]
```

### Examples

```
  qurl mint r_k8xqp9h2sj9
  LINK=$(qurl mint r_k8xqp9h2sj9 -q)
```

### Options

```
  -h, --help   help for mint
```

### Options inherited from parent commands

```
      --api-key string    API key (prefer env var or config file to avoid exposure in process list)
      --endpoint string   API endpoint (default: https://api.layerv.ai)
  -o, --output string     Output format: table or json (default "table")
      --profile string    Config profile name (reads ~/.config/qurl/profiles/NAME.yaml)
  -q, --quiet             Minimal output (just the essential value)
  -v, --verbose           Show HTTP request/response details
```

### SEE ALSO

* [qurl](qurl.md)	 - qURL CLI - manage secure links from the command line
