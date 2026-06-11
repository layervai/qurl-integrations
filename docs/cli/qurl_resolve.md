## qurl resolve

Resolve a qURL access token (headless)

### Synopsis

Resolve a qURL access token to get the target URL and grant network access.
After resolution, the target URL is accessible from your IP for the duration
specified in the access grant.

The access token can be provided as an argument, via stdin, or interactively:
  qurl resolve at_abc123           Argument (visible in shell history)
  echo $TOKEN | qurl resolve       Stdin (safer)
  qurl resolve                     Interactive prompt (hidden input)

```
qurl resolve [ACCESS_TOKEN] [flags]
```

### Examples

```
  qurl resolve at_k8xqp9h2sj9lx7r4a
  echo "$TOKEN" | qurl resolve
  qurl resolve -o json
```

### Options

```
  -h, --help   help for resolve
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
