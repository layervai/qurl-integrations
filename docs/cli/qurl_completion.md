## qurl completion

Generate shell completion scripts

### Synopsis

Generate shell completion scripts for qurl.

  Bash:   eval "$(qurl completion bash)"
  Zsh:    qurl completion zsh > "${fpath[1]}/_qurl"
  Fish:   qurl completion fish > ~/.config/fish/completions/qurl.fish

```
qurl completion [bash|zsh|fish|powershell] [flags]
```

### Options

```
  -h, --help   help for completion
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
