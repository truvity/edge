# static-site-infra

The cloud identity of one [static-site](../static-site) install on EKS, as its
own chart (platform contract rule 11, decision 0013 of truvity/policy). In
order, by sync wave:

| wave | object |
|---|---|
| -30 | ACK IAM `Policy`: the ECR actions the server calls |
| -25 | ACK IAM `Role`, trusting `pods.eks.amazonaws.com` for this namespace and ServiceAccount |
| -20 | ACK EKS `PodIdentityAssociation` |
| -15 | the `ServiceAccount`, with `argocd.argoproj.io/sync-options: Prune=false,Delete=false` |
| | with `networkPolicy.enabled`: a `NetworkPolicy` for the egress of static-site's identity check |

Install it, with the same release name (or `installName`), and wait until it is
healthy **before** installing or upgrading `static-site`: that chart's
pre-install hook runs as this chart's ServiceAccount. It renders nothing unless
`registry.auth=ecr` and `registry.ack.enabled=true`. The objects are those the
`static-site` chart rendered before this chart existed, with the same names and specs.

Values: `registry.auth`, `registry.ack.{enabled,clusterName,repositoryArns,rolePrefix,accountId,region,tags,permissionsBoundary}`,
`serviceAccount.{create,name,annotations}`, `networkPolicy.enabled`,
`installName`, `nameOverride`, `fullnameOverride`. The names are those of the
`static-site` chart. `registry.ack.rolePrefix` may hold a dot, for a platform that
names project roles `{cluster}-{project}.{name}` (`example-site.static-www`): the dot
is allowed in an IAM name and in the names of the ACK objects, which carry it
unchanged (the case `project-role-name`). See [docs/static-site.md](../../docs/static-site.md).

```sh
helm upgrade --install www oci://ghcr.io/truvity/charts/static-site-infra \
  --namespace static --create-namespace -f infra-values.yaml
```
