# nodetaint

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.0.3](https://img.shields.io/badge/AppVersion-v0.0.3-informational?style=flat-square)

A Helm chart for nodetaint controller

## How to install this chart


Add wish.github.io/nodetaint chart repo:

```console
helm repo add nodetaint https://wish.github.io/nodetaint
```

A simple install with default values:

```console
helm install nodetaint/nodetaint
```

To install the chart with the release name `my-release`:

```console
helm install my-release nodetaint/nodetaint
```

To install with some set values:

```console
helm install my-release nodetaint/nodetaint --set values_key1=value1 --set values_key2=value2
```

To install with custom values file:

```console
helm install my-release nodetaint/nodetaint -f values.yaml
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` |  |
| config.DAEMONSET_ANNOTATION | string | `"nodetaint/crucial"` |  |
| config.LOG_LEVEL | string | `"info"` |  |
| config.NODE_TAINT | string | `"nodetaint/blocking"` |  |
| fullnameOverride | string | `""` |  |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| image.registry | string | `"quay.io/"` |  |
| image.repository | string | `"wish/nodetaint"` |  |
| image.tag | string | `"v0.0.3"` |  |
| imagePullSecrets | list | `[]` |  |
| nameOverride | string | `""` |  |
| nodeSelector | object | `{}` |  |
| podAnnotations | object | `{}` |  |
| podDisruptionBudget.maxUnavailable | int | `1` |  |
| podSecurityContext | object | `{}` |  |
| rbac.create | bool | `true` |  |
| resources | object | `{}` |  |
| securityContext | object | `{}` |  |
| service.port | int | `80` |  |
| service.type | string | `"ClusterIP"` |  |
| serviceAccount.annotations | object | `{}` |  |
| serviceAccount.create | bool | `true` |  |
| serviceAccount.name | string | `""` |  |
| tolerations | list | `[]` |  |

## How to build and release this chart

Chart is built and released on each merge to `main` branch.

Release version is stored in `Chart.yaml` and should be bumped with each release

Before pushing your commits, make sure to regenerate docs with:
```console
docker run --rm --volume "$(pwd):/helm-docs" -u $(id -u) jnorwood/helm-docs:latest --template-files=ci/README.md.gotmpl
```
