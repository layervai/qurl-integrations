## qurl delete

Revoke/delete a qURL

```
qurl delete RESOURCE_ID [flags]
```

### Examples

```
  qurl delete r_k8xqp9h2sj9 --yes
```

### Options

```
      --dry-run   Show what would be done without making changes
  -h, --help      help for delete
  -y, --yes       Skip confirmation prompt
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
