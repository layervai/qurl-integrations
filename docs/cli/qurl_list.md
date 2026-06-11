## qurl list

List qURLs

```
qurl list [flags]
```

### Examples

```
  qurl list
  qurl list --status active --limit 50
  qurl list --sort created_at:desc
  qurl list --query "dashboard"
```

### Options

```
      --cursor string   Pagination cursor from a previous list response
  -h, --help            help for list
  -l, --limit int       Maximum number of qURLs to return (default 20)
      --query string    Search description and target URL
      --sort string     Sort field:direction (e.g., created_at:desc)
      --status string   Filter by status (active, expired, revoked, consumed)
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
