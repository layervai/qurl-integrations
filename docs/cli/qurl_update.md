## qurl update

Update a qURL's properties

```
qurl update RESOURCE_ID [flags]
```

### Examples

```
  qurl update r_k8xqp9h2sj9 --description "Production API access"
  qurl update r_k8xqp9h2sj9 -d ""  # clear description
```

### Options

```
  -d, --description string   New description (use empty string to clear)
  -h, --help                 help for update
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
