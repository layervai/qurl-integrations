## qurl create

Create a qURL for a target URL

```
qurl create TARGET_URL [flags]
```

### Examples

```
  qurl create https://api.example.com/data
  qurl create https://internal.example.com --expires 1h --one-time
  qurl create https://dashboard.example.com --label "Admin access" -e 7d
```

### Options

```
  -e, --expires string     Expiration duration (e.g., 1h, 24h, 7d)
  -h, --help               help for create
      --label string       Human-readable label identifying who this qURL is for
      --max-sessions int   Maximum concurrent sessions (0 = unlimited)
      --one-time           Single-use token (consumed after first access)
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
