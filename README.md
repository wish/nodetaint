# nodetaint

Taint controller for nodes in k8s cluster. 

## How it works

You need to configure your cluster to launch nodes with desired taint. Controller maintains a list of required daemonset which are system critical and need to be running before other pods can be scheduled on a node. It monitors and waits for the daemonset pods to be ready on a node upon node startup and remove the taints on a node once all the required daemonset pods are ready.

## Configuration

### Command-line

`nodetaint` can be configured by the following command-line options:

Flag | Environment Variable | Type | Default | Required | Description
---- | -------------------- | ---- | ------- | -------- | -----------
`log-level` | `LOG_LEVEL` | `string` | `info` | no | The level of log detail.
`bind-address` | `BIND_ADDRESS` | `string` | `:9797` | no | The address for binding listener.
`node-taint` | `NODE_TAINT` | `string` | | yes |  The startup taint to put on node.
`daemonset-annotation` | `DAEMONSET_ANNOTATION` | `string` | | yes | The annotation of required daemonset.
 
 