## qurl config set

Set a configuration value

```
qurl config set KEY VALUE [flags]
```

### Examples

```
  qurl config set api_key lv_live_xxx
  qurl config set --profile staging api_key lv_live_yyy
```

### Options

```
  -h, --help             help for set
      --profile string   Profile name to configure
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
