## qurl config get

Get a configuration value

```
qurl config get KEY [flags]
```

### Examples

```
  qurl config get endpoint
```

### Options

```
  -h, --help             help for get
      --profile string   Profile name to read
```

### Options inherited from parent commands

```
      --api-key string    API key (prefer env var or config file to avoid exposure in process list)
      --endpoint string   API endpoint (default: https://api.layerv.ai)
  -o, --output string     Output format: table or json (default "table")
  -q, --quiet             Minimal output (just the essential value)
  -v, --verbose           Show HTTP request/response details
```

### SEE ALSO

* [qurl config](qurl_config.md)	 - Manage CLI configuration
