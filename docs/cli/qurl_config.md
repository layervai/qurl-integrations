## qurl config

Manage CLI configuration

### Synopsis

Manage CLI configuration stored at ~/.config/qurl/config.yaml.

Supported keys: api_key, endpoint, output

Profiles:
  Use --profile to manage named profiles stored under ~/.config/qurl/profiles/.
  qurl config set --profile staging api_key lv_live_yyy
  qurl --profile staging list

### Options

```
  -h, --help   help for config
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
* [qurl config get](qurl_config_get.md)	 - Get a configuration value
* [qurl config path](qurl_config_path.md)	 - Show config file path
* [qurl config profiles](qurl_config_profiles.md)	 - List available configuration profiles
* [qurl config set](qurl_config_set.md)	 - Set a configuration value
