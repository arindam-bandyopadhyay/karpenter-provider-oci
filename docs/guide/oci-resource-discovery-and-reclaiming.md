# OCI Resource Discovery and Reclaiming

Customers are generally responsible for terminating instances and resources provisioned by KPO (especially during cluster deletion or when cleaning up after tests).

### In-cluster cleanup

1. **List Karpenter-managed capacity**:

```shell
kubectl get nodes -l karpenter.sh/nodepool
```

or:

```shell
kubectl get nodeclaims -o wide
```

2. **Delete the NodePool(s)** (this will deprovision nodes over time):

```shell
kubectl get nodepools
kubectl delete nodepool <nodepool-name>
```

3. **Wait for NodeClaims to drain/delete**:

```shell
kubectl get nodeclaims
```

If you need to force cleanup (for example, after partial failures), inspect finalizers and controller logs before removing finalizers.

### OCI-side discovery

Instances launched by Karpenter will have `KarpenterNodePool` and `KarpenterNodePoolUID` freeform tags. `KarpenterNodePoolUID` identifies the Kubernetes NodePool that launched the instance. You can use:
- **OCI Resource Explorer** to find instances by tags: [Querying resources](https://docs.oracle.com/en-us/iaas/Content/Search/Tasks/queryingresources.htm)
- **OCI CLI Search** (optional) to list resources by structured search:

```shell
oci search resource structured-search --query-text \
  "query instance resources where (freeformTags.key = 'KarpenterNodePool')"
```

Instances created by older KPO versions might not have `KarpenterNodePoolUID`. KPO falls back to the legacy `KarpenterNodePool` name match for those instances.

When deleting a cluster, ensure that any Karpenter-managed instances (and any dependent resources they created) are also deleted.
