# nodetaint

Controller to manage taints for nodes in a k8s cluster.

## What the problem is

Usually there are some system critical daemonsets (e.g. CNI, DNS, etc.) that needs to be running on a node before it can run any other pods. However k8s doesn't guarantee any ordering for pod scheduling in node startup, meaning your workload pods may start before the critical daemonsets have started!

## How it works

The controller solves this problem by removing a pre-configured taint from a node after annotated daemonsets are running on the node. To achieve this, you need to configure your cluster to launch nodes with the desired taint: configure `kubelet` to start with `--register-with-taints` option.
 The controller then determines, through annotation, which daemonsets should be running on a node prior to workload pods. It monitors for these daemonset pods to be Ready before removing the configured taint.

## Configuration

### Command-line

`nodetaint` can be configured by the following command-line options:

Flag | Environment Variable | Type | Default | Required | Description
---- | -------------------- | ---- | ------- | -------- | -----------
`log-level` | `LOG_LEVEL` | `string` | `info` | no | The level of log detail.
`bind-address` | `BIND_ADDRESS` | `string` | `:9797` | no | The address for binding listener.
`node-taint` | `NODE_TAINT` | `string` | | yes |  The startup taint to put on node.
`daemonset-annotation` | `DAEMONSET_ANNOTATION` | `string` | | yes | The annotation of required daemonset.
 
 
