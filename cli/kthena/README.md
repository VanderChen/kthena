# kthena - kthena CLI

`kthena` is a command-line interface tool for managing kthena AI inference workloads in Kubernetes clusters.

> **ModelBooster deprecation:** ModelBooster is deprecated in v1.1; use ModelServing, ModelServer, and ModelRoute. Removal is no earlier than v1.5. ModelBooster commands and templates remain available for existing users. For new deployments, follow the [ModelServing quick start](../../docs/kthena/docs/getting-started/quick-start.md#modelserving).

## Architecture Diagrams

### Use Case Diagram

```plantuml

actor "User" as user

package "kthena CLI" {
  
  package "Verb Layer" as verbLayer{
    usecase "Get" as GetVerb
    usecase "Describe" as DescribeVerb
    usecase "Create" as CreateVerb
  }
  
  package "Resource Layer" as ResourceLayer {
    usecase "Template(s)" as Template
    usecase "Manifest" as Manifest
    
    rectangle "Kubernetes Resources" as KubernetesResources {
       usecase "Model(s)" as ModelResource
       usecase "ModelServing(s)" as ModelServingResource
       usecase "Policy(s)" as PolicyResource
       usecase "PolicyBinding(s)" as PolicyBindingResource
    }
  }
  
  package "Flag Layer" as FlagLayer{
    usecase "--dry-run" as DryRunFlag
    usecase "--namespace" as NamespaceFlag
  }
}

' User interactions with verb layer (kubectl-style)
user --> GetVerb
user --> DescribeVerb
user --> CreateVerb

' Verb layer connects to resource layer
CreateVerb --> Manifest
GetVerb --> Template
GetVerb --> KubernetesResources

DescribeVerb --> Template
DescribeVerb --> KubernetesResources

' Resources can use flags
Manifest --> NamespaceFlag
Manifest --> DryRunFlag

note top of CreateVerb
  Create Kthena resources from
  templates with custom values
end note

' layout
GetVerb -[hidden]-> ModelResource
GetVerb -[hidden]-> ModelServingResource
GetVerb -[hidden]-> PolicyResource
GetVerb -[hidden]-> PolicyBindingResource

verbLayer -[hidden]-> ResourceLayer
ResourceLayer -[hidden]-> FlagLayer
```

## Overview

The `kthena` CLI follows kubectl-style verb-noun grammar and provides an easy way to:
- Get and view templates and Kthena resources in your cluster
- Describe detailed information about specific templates and resources
- Create Kthena resources from predefined manifest templates
- Check ModelServings before an upgrade of the controller manager, and verify the upgrade afterwards
- Manage inference workloads, models, and autoscaling policies with kubectl-like commands

### Install

```bash
go install github.com/volcano-sh/kthena/cli/kthena@latest
```

### Build from Source

```bash
# From the project root directory
go build -o bin/kthena cli/kthena/main.go
```

### Add to PATH (Optional)

```bash
# Add the bin directory to your PATH
export PATH=$PATH:$(pwd)/bin
```

## Usage

The `kthena` CLI follows kubectl-style verb-noun grammar for consistency and ease of use.

### Template Operations

List all available templates:
```bash
kthena get templates
```

Get a specific template content:
```bash
kthena get template deepseek-r1-distill-llama-8b
kthena get template deepseek-r1-distill-llama-8b -o yaml
```

Describe a template with detailed information:
```bash
kthena describe template DeepSeek-R1-Distill-Qwen-32B
```

### Resource Operations

List models:
```bash
kthena get model-boosters
kthena get model-boosters --all-namespaces
kthena get model-boosters -n production
```

List model serving workloads:
```bash
kthena get model-servings
kthena get model-servings --all-namespaces
```

List autoscaling policies:
```bash
kthena get autoscaling-policies
kthena get autoscaling-policies -n production
```

### Creating Resources

Create resources from templates:
```bash
kthena create manifest --template deepseek-r1-distill-llama-8b --name my-model
kthena create manifest --template deepseek-r1-distill-llama-8b --values-file values.yaml
kthena create manifest --template deepseek-r1-distill-llama-8b --name my-model --dry-run
```

### Upgrade Pre-flight Check

The version of Kthena that keeps every ServingGroup of a ModelServing in a `ServingGroup` object takes over the
Pods, PodGroups and Services that earlier versions own through the ModelServing, without restarting a Pod.
`kthena preflight model-servings` tells, before the upgrade, what the new controllers will do with every
ModelServing, and verifies the upgrade afterwards. Run it against the cluster while it still runs the earlier
version.

```bash
# 1. Before the upgrade: check, and record the baseline for the verification
kthena preflight model-servings --all-namespaces --baseline-file baseline.json

# 2. Apply the CRDs of the new chart by hand, then helm upgrade

# 3. After the upgrade, when every ModelServing is taken over: verify
kthena preflight model-servings --compare-baseline baseline.json
```

```text
kthena preflight model-servings [NAME] [-n NAMESPACE | -A] [-o table|json]
                                [--baseline-file FILE | --compare-baseline FILE] [--skip-dry-run]
```

The command follows the verb-noun grammar of `get` and `describe`: `preflight` is the verb, `model-servings`
(aliases `ms`, `model-serving`) the resource. The design of the ServingGroup resource refers to it as
`modelserving preflight`. `-n`, `-A` and `-o` work as they do for `kthena get`; without them the namespace of
the current kubeconfig context is checked. The upgrade replaces the controller of every namespace, so use `-A`
before upgrading.

| Flag | Meaning |
| --- | --- |
| `-o table` (default) | Report for a person: the cluster, one line per ModelServing, and the findings behind every verdict |
| `-o json` | The same report with stable field names (`formatVersion: v1`); the baseline notice goes to standard error |
| `--baseline-file FILE` | Also write the baseline: per Pod the UID, the restart count of every container, the Ready condition and its last transition time, and the node; per PodGroup and headless Service the UID; per ModelServing the status counts and revisions |
| `--compare-baseline FILE` | Instead of the check, compare the cluster with a baseline. The scope is the one of the baseline |
| `--skip-dry-run` | Leave out the dry-run patch (see below) |

The check only reads, with one exception: every ModelServing is patched once with a **server-side dry-run** that
adds the annotation `modelserving.volcano.sh/servinggroup-adoption`, the write the takeover begins with. The API
server runs admission and validation and persists nothing. It needs `get` and `list` on ModelServings,
ServingGroups, Pods, Services, PodGroups, ControllerRevisions and PodDisruptionBudgets in the checked
namespaces, `patch` on ModelServings for the dry-run, and, cluster-wide, `list` on Deployments, Pods and Nodes
and `get` on ConfigMaps and ValidatingWebhookConfigurations. A cluster-level read that is not permitted is
reported as a finding and does not stop the check.

Every ModelServing, and the cluster, gets a verdict: the most severe level among its findings.

| Verdict | Meaning | Exit code |
| --- | --- | --- |
| `ready` | Can be upgraded without disruption | 0 |
| `attention` | The upgrade works, and something happens that you should know about | 2 |
| `wait` | Something is in progress; the finding says what to wait for | 2 |
| `blocked` | Would not be taken over cleanly; the finding says why | 3 |

Exit code 1 means that the check could not run. With `--compare-baseline` the exit code is 0 when nothing that
the baseline records has changed, 3 when something changed that must not change (a Pod replaced or restarted,
a PodGroup or Service replaced, `status.availableReplicas` lower, a ModelServing not taken over, a group without
its ServingGroup), and 2 when the only changes are in ModelServings for which the check announced that the
controller changes Pods by itself (`podChangesExpected` in the report), or whose spec was changed since.

Findings about the cluster:

| Finding | Level | Meaning |
| --- | --- | --- |
| `controller-manager-in-several-namespaces` | blocked | Controller manager Pods or Deployments exist in more than one namespace; the new version only waits for an earlier version in its own namespace |
| `rollback-switch-present` | blocked | The ConfigMap `kthena-servinggroup-rollback` exists in the namespace of the controller manager: the new version would start in rollback mode |
| `controller-manager-upgrade-in-progress` | wait | Controller manager Pods of both versions exist |
| `controller-manager-upgraded` | attention | The controller manager already works with ServingGroup objects |
| `controller-manager-not-running`, `controller-manager-not-found` | attention | No controller manager Pod runs, or none is found in any namespace |
| `controller-manager-recreate-strategy` | attention | The Deployment uses the update strategy Recreate: no webhook answers during the upgrade |
| `leader-election` | info, attention | Whether `--leader-elect` is set or defaulted; attention with several replicas and no leader election |
| `read-failed` | attention | A cluster-level read was not permitted; what depends on it was not checked |
| `servinggroup-crd-not-installed` | info | The ServingGroup CRD has to be applied by hand before `helm upgrade` |
| `servinggroups-exist`, `cordoned-nodes`, `scope-limited` | info | ServingGroup objects that exist and who controls them; cordoned nodes; only part of the cluster was checked |

Findings about a ModelServing:

| Finding | Level | Meaning |
| --- | --- | --- |
| `revision-identity` | attention | **k groups (Role instances under RoleRollingUpdate) will be rolled after the upgrade** although the spec was not changed: the new version resolves the template to a revision that does not render the same Pods as the one the running version rolled to. Without this finding the report says "no rollout after the upgrade" |
| `rollout-in-progress` | wait | Groups or Role instances are not at the target revision yet; the new version continues the rollout |
| `partition-holds` | info | Groups or Role instances below the partition stay at their revision |
| `revision-unknown` | attention | The revision of a group cannot be compared with the target (ControllerRevision missing, or Pods of two revisions): it is taken over and not rolled |
| `revision-unresolved` | blocked | The revision of the template cannot be computed |
| `groups-exceed-replicas` | wait | **More groups without the surge mark than `spec.replicas`**: after the upgrade one group per surplus is scaled in; the finding names the likely one (not ready first, then lowest Pod deletion cost, then highest ordinal) |
| `groups-below-replicas` | wait | Fewer groups than `spec.replicas`: a scale-out or a recovery is in progress |
| `surge-groups` | wait | Temporary surge groups of a rolling update exist; they are taken over as such and removed when the rollout ends |
| `surge-mark-partial` | attention | Only some Pods of a group carry the surge mark; one is enough to make it a surge group |
| `surge-mark-below-replicas`, `unmarked-group-beyond-replicas`, `surge-mark-ignored`, `ordinal-holes` | info | Where surge marks are and what the takeover makes of them; ordinals that are not contiguous |
| `role-surge-instances` | wait, attention | Temporary Role instances: of a Role rollout in progress (wait), or left over under ServingGroupRollingUpdate (attention) |
| `terminating-pods`, `inplace-update-in-progress`, `not-observed` | wait | Pods terminating; an in-place update in progress; a spec the running controller has not processed |
| `failed-pods`, `restarting-pods` | wait, attention | Pods in phase Failed, or not ready with restarted containers: the scope that `recoveryPolicy` recreates (wait), or tolerated when recovery is disabled (attention) |
| `pod-not-a-member` | blocked | A Pod that the ModelServing controls has not the name of a member (`<group>-<role>-<instance>-<member>`): it is not taken over, and the takeover does not finish |
| `pod-without-owner`, `pod-foreign-owner` | attention | Labelled Pods without a controller owner (adopted when they have the name of a member, left alone otherwise), or controlled by another object |
| `servinggroup-name-taken` | blocked | A ServingGroup object holds the name of a group without being controlled by the ModelServing |
| `dry-run-rejected` | blocked | An admission webhook or the API server rejects the write of the adoption annotation |
| `dry-run-rejected-by-kthena-webhook`, `dry-run-unknown` | attention | The running version's own webhook rejects the object (the new version admits updates that leave the spec unchanged), or the dry-run could not be judged |
| `pods-on-cordoned-nodes`, `disruption-budget-expected-count` | attention | Pods on cordoned nodes; PodDisruptionBudgets whose allowed disruptions change with the owner of the Pods |
| `objects-of-groups-without-pods` | info | PodGroups and Services of a group without a Pod: the takeover leaves them to the ModelServing |
| `deleting`, `takeover-in-progress`, `rollback-in-progress` | wait | The ModelServing is being deleted, taken over, or handed back |
| `taken-over` | info | The ModelServing was taken over already |

For more detailed usage information, run:
```bash
kthena --help
kthena get --help
kthena describe --help
kthena create --help
kthena preflight model-servings --help
```

## Configuration

The CLI uses your local kubectl configuration. Ensure you have:
- Valid kubeconfig file (usually `~/.kube/config`)
- Access to a Kubernetes cluster with Kthena CRDs installed
- Appropriate RBAC permissions for the target namespaces

## Contributing

To add new manifest templates:

1. Create a new `.yaml` file under `helm/templates/<vendor>/` (templates are embedded into the binary at build time, so rebuild the CLI to pick it up)
2. Use Go template syntax with variables: `{{.variable_name}}`
3. Add a description comment at the top: `# Description: Your template description`
4. Test with `kthena describe template your-template`

Example template structure:
```yaml
# Description: Your template description
# Variables: var1, var2, var3
---
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: {{.name}}
  namespace: {{.namespace}}
spec:
  # ... template content
```

## About Cobra

Kthena CLI is built with [Cobra](https://github.com/spf13/cobra).
The building blocks of cobra are depicted as below.

```plantuml
!theme plain
skinparam linetype ortho
skinparam nodesep 10
skinparam ranksep 20

' Main tree structure
rectangle "**Cobra CLI Application**\n//Entry Point//" as App #lightblue

rectangle "**Root Command**\n//cobra.Command//\n- Entry point\n- Execute()\n- Global configuration" as Root #lightgreen

rectangle "**Sub Command 1**\n//cobra.Command//\n- Specific functionality\n- Inherits from parent" as Sub1 #lightgreen

rectangle "**Sub Command 2**\n//cobra.Command//\n- Different functionality\n- Can have own subcommands" as Sub2 #lightgreen

rectangle "**Sub-Sub Command**\n//cobra.Command//\n- Nested functionality\n- Deep command hierarchy" as SubSub #lightgreen

' Flags branch
rectangle "**Flags**\n//Command Options//" as FlagsRoot #lightyellow
rectangle "**Persistent Flags**\n- Available to all subcommands\n- Inherited down the tree\n- Global configuration" as PFlags #lightyellow
rectangle "**Local Flags**\n- Command-specific only\n- Not inherited\n- Local configuration" as LFlags #lightyellow

' Arguments branch
rectangle "**Arguments**\n//Positional Parameters//" as ArgsRoot #lightcyan
rectangle "**Validation Rules**\n- NoArgs\n- MinimumNArgs(n)\n- ExactArgs(n)\n- Custom validation" as ArgValidation #lightcyan
rectangle "**Argument Values**\n- []string args\n- Parsed by cobra\n- Passed to Run function" as ArgValues #lightcyan

' Hooks/Lifecycle branch
rectangle "**Lifecycle Hooks**\n//Execution Flow//" as Hooks #lightpink
rectangle "**Pre-Execution**\n- PersistentPreRun\n- PreRun\n- Setup and validation" as PreHooks #lightpink
rectangle "**Execution**\n- Run function\n- Main command logic\n- Core functionality" as RunHook #lightpink
rectangle "**Post-Execution**\n- PostRun\n- PersistentPostRun\n- Cleanup and finalization" as PostHooks #lightpink

' External Integration branch
rectangle "**External Integration**\n//Third-party Libraries//" as External #lavender
rectangle "**Viper**\n- Configuration management\n- File/env/flag binding\n- Hot reloading" as Viper #lavender
rectangle "**Pflag**\n- POSIX-compliant flags\n- Flag parsing engine\n- Type conversion" as Pflag #lavender

' Features branch
rectangle "**Features**\n//Built-in Capabilities//" as Features #wheat
rectangle "**Auto Help**\n- Generated help text\n- Usage information\n- Command discovery" as Help #wheat
rectangle "**Shell Completion**\n- Bash completion\n- Zsh completion\n- PowerShell completion" as Completion #wheat
rectangle "**Error Handling**\n- Command validation\n- Flag validation\n- Custom error messages" as ErrorHandling #wheat

' Main tree connections
App ||--|| Root : "starts with"
Root ||--o{ Sub1 : "contains"
Root ||--o{ Sub2 : "contains"
Sub1 ||--o{ SubSub : "can contain"

' Flags connections
Root ||--|| FlagsRoot : "has"
FlagsRoot ||--|| PFlags : "includes"
FlagsRoot ||--|| LFlags : "includes"

' Arguments connections
Root ||--|| ArgsRoot : "validates"
ArgsRoot ||--|| ArgValidation : "applies"
ArgsRoot ||--|| ArgValues : "processes"

' Hooks connections
Root ||--|| Hooks : "executes"
Hooks ||--|| PreHooks : "runs first"
Hooks ||--|| RunHook : "runs main"
Hooks ||--|| PostHooks : "runs last"

' External connections
Root ||..|| External : "integrates"
External ||--|| Viper : "uses"
External ||--|| Pflag : "built on"

' Features connections
Root ||--|| Features : "provides"
Features ||--|| Help : "generates"
Features ||--|| Completion : "supports"
Features ||--|| ErrorHandling : "handles"

' Inheritance flows (dotted lines for inheritance)
Sub1 -.-> Root : "inherits flags & hooks"
Sub2 -.-> Root : "inherits flags & hooks"
SubSub -.-> Sub1 : "inherits flags & hooks"

note top of App
**Execution Flow:**
main() → rootCmd.Execute() → 
flag parsing → argument validation → 
hook execution → command logic
end note

note bottom of PFlags
**Flag Inheritance:**
Persistent flags flow down
the command tree to all
child commands
end note

note right of Hooks
**Hook Execution Order:**
1. PersistentPreRun (parent)
2. PreRun (current command)
3. Run (main logic)
4. PostRun (current command)
5. PersistentPostRun (parent)
end note

```
