---
title: Kthena CLI
---
## kthena preflight

Check resources before an upgrade of Kthena

### Synopsis

Check resources before an upgrade of Kthena, and verify the upgrade afterwards.

The checks read the cluster and change nothing.

Examples:
  kthena preflight model-servings --all-namespaces
  kthena preflight model-servings -A --baseline-file baseline.json
  kthena preflight model-servings --compare-baseline baseline.json

### Options

```
  -A, --all-namespaces     Check resources across all namespaces
  -h, --help               help for preflight
  -n, --namespace string   Kubernetes namespace (default: current context namespace)
  -o, --output string      Output format (table|json)
```

### SEE ALSO

* [kthena](kthena.md)	 - Kthena CLI for managing AI inference workloads
* [kthena preflight model-servings](kthena_preflight_model-servings.md)	 - Check ModelServings before the upgrade to ServingGroup objects

