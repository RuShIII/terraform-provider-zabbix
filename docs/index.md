---
page_title: "zabbix Provider"
subcategory: ""
description: |-
  Terraform/OpenTofu provider for managing Zabbix objects via the JSON-RPC API.
---

# zabbix Provider

The `zabbix` provider lets you manage Zabbix resources (hosts, host groups, templates, triggers)
from Terraform/OpenTofu.

## Supported resources

- `zabbix_host`
- `zabbix_host_group`
- `zabbix_template`
- `zabbix_trigger`

## Data sources

This provider currently does not expose any data sources.

## Example Usage

```terraform
terraform {
  required_providers {
    zabbix = {
      source  = "rushiii/zabbix"
      version = "1.0.0"
    }
  }
}

provider "zabbix" {
  url       = "https://zabbix.example.com/api_jsonrpc.php"
  api_token = var.zabbix_api_token
}
```

Example with username/password authentication:

```terraform
provider "zabbix" {
  url      = "https://zabbix.example.com/api_jsonrpc.php"
  username = var.zabbix_username
  password = var.zabbix_password
}
```

## Schema

### Required

- `url` (String) Zabbix API URL, for example `https://zabbix.example.com/api_jsonrpc.php`.

### Optional

- `api_token` (String, Sensitive) Zabbix API token. Takes priority if provided.
- `username` (String) Zabbix username (used when `api_token` is not set).
- `password` (String, Sensitive) Zabbix password (used when `api_token` is not set).
- `timeout_seconds` (Number) HTTP timeout in seconds. Default: `30`.
- `insecure_skip_tls` (Boolean) Skip TLS certificate validation (for lab/testing only).

## Authentication behavior

- If `api_token` is set, it is used first.
- If `api_token` is empty, `username` and `password` are required.
- If both `api_token` and `username/password` are set, the token still takes priority.

## Configure-time validation

During provider initialization:

1. The HTTP/API client is initialized.
2. An `apiinfo.version` ping is executed to verify API reachability.
3. If the check fails, planning/apply fails immediately.
