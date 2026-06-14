## qurl extend

Extend qURL expiration

```
qurl extend RESOURCE_ID [flags]
```

### Examples

```
  qurl extend r_k8xqp9h2sj9 --by 24h
```

### Options

```
  -b, --by string   Duration to extend by (e.g., 1h, 24h, 7d)
  -h, --help        help for extend
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
