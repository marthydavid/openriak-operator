#!/bin/bash
# Integration test for OpenRiak Operator
# This script tests the operator with a Kind cluster

set -e

CLUSTER_NAME="riak-operator-test"
NAMESPACE="default"

echo "Creating Kind cluster: $CLUSTER_NAME"
kind create cluster --name "$CLUSTER_NAME" --wait 5m

echo "Loading operator image into Kind"
make docker-build IMG=riak-operator:test
kind load docker-image riak-operator:test --name "$CLUSTER_NAME"

echo "Installing CRDs and deploying operator"
make install
make deploy IMG=riak-operator:test

echo "Waiting for operator to be ready"
kubectl rollout status deployment/manager -n riak-system --timeout=5m

echo "Deploying test cluster"
kubectl apply -f examples/1-dev-cluster.yaml

echo "Waiting for cluster to reach Ready phase (this may take 5-10 minutes)"
for i in {1..60}; do
    PHASE=$(kubectl get riakcluster riak-cluster-dev -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    READY=$(kubectl get riakcluster riak-cluster-dev -o jsonpath='{.status.readyNodes}' 2>/dev/null || echo "0")
    echo "[$i/60] Phase: $PHASE, Ready Nodes: $READY/3"
    
    if [ "$PHASE" = "Ready" ] && [ "$READY" = "3" ]; then
        echo "✓ Cluster is ready!"
        break
    fi
    sleep 10
done

if [ "$PHASE" != "Ready" ]; then
    echo "✗ Cluster failed to reach Ready state"
    echo "Cluster status:"
    kubectl describe riakcluster riak-cluster-dev
    echo ""
    echo "Operator logs:"
    kubectl logs -n riak-system -l app.kubernetes.io/name=openriak-operator --tail=50
    exit 1
fi

echo "Creating buckets"
kubectl apply -f examples/4-buckets.yaml

echo "Checking bucket creation"
sleep 5
BUCKETS=$(kubectl get riakbuckets -o jsonpath='{.items[*].status.created}')
if [[ "$BUCKETS" == *"true"* ]]; then
    echo "✓ Buckets created successfully"
else
    echo "✗ Bucket creation failed"
    kubectl describe riakbucket
    exit 1
fi

echo "Creating user"
kubectl apply -f examples/3-user-with-grants.yaml

echo "Checking user creation"
sleep 5
USER_STATUS=$(kubectl get riakuser app-user -o jsonpath='{.status.phase}')
if [ "$USER_STATUS" = "Ready" ]; then
    echo "✓ User created successfully"
else
    echo "✗ User creation failed"
    kubectl describe riakuser app-user
    exit 1
fi

echo ""
echo "=================================="
echo "✓ All tests passed!"
echo "=================================="
echo ""
echo "To access the cluster:"
echo "  kubectl port-forward svc/riak-cluster-dev 8087:8087 8098:8098"
echo ""
echo "To clean up:"
echo "  kind delete cluster --name $CLUSTER_NAME"
