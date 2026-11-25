---
title: Kthena CLI
---
## kthena preflight model-servings

Check ModelServings before the upgrade to ServingGroup objects

### Synopsis

Check ModelServings before the controller manager is upgraded to the version
that keeps every ServingGroup of a ModelServing in a ServingGroup object.

The earlier versions own the Pods, PodGroups and Services of a ModelServing
directly; the new version takes them over without restarting a Pod. Run this
check against the cluster while it still runs the earlier version. It reports
on the cluster (controller manager, ServingGroup CRD, existing ServingGroup
objects, cordoned nodes, rollback switch) and gives every ModelServing a verdict:

  ready      can be upgraded without disruption
  attention  the upgrade works, and something happens that you should know about
  wait       something is in progress; the finding says what to wait for
  blocked    the ModelServing would not be taken over cleanly; the finding says why

The check is read-only with one exception: every ModelServing is patched once
with a server-side dry-run that adds the annotation
modelserving.volcano.sh/servinggroup-adoption. The API server admits and
validates the patch and persists nothing. It shows whether an admission webhook
or a policy would reject the write the takeover begins with. --skip-dry-run
leaves it out.

--baseline-file records the Pods (UID, restart counts, readiness, node), the
PodGroups and headless Services (UID) and the status of every ModelServing.
After the upgrade, --compare-baseline checks the cluster against that record:
every ModelServing is taken over, every group with Pods has its ServingGroup,
no Pod, PodGroup or Service was replaced, no container restarted, and
status.availableReplicas did not drop. The scope of the comparison is the scope
of the baseline.

Exit codes:
  0  every ModelServing and the cluster are ready; or the comparison found no change
  1  the check could not run
  2  the worst verdict is wait or attention; or the comparison found only changes
     that the pre-flight check announced or a change of the spec explains
  3  a verdict is blocked; or the comparison found a change that must not happen

Examples:
  kthena preflight model-servings --all-namespaces
  kthena preflight model-servings -n production -o json
  kthena preflight model-servings my-serving -n production --skip-dry-run
  kthena preflight model-servings -A --baseline-file baseline.json
  kthena preflight model-servings --compare-baseline baseline.json

```
kthena preflight model-servings [NAME] [flags]
```

### Options

```
      --baseline-file string      Write the baseline for the verification after the upgrade to this file
      --compare-baseline string   Compare the cluster with the baseline in this file instead of running the pre-flight check
  -h, --help                      help for model-servings
      --skip-dry-run              Do not send the dry-run patch that tells whether the adoption annotation can be written
```

### Options inherited from parent commands

```
  -A, --all-namespaces     Check resources across all namespaces
  -n, --namespace string   Kubernetes namespace (default: current context namespace)
  -o, --output string      Output format (table|json)
```

### SEE ALSO

* [kthena preflight](kthena_preflight.md)	 - Check resources before an upgrade of Kthena

