# Contributing to OpenRiak Operator

We welcome contributions! This guide will help you get started.

## Development Setup

### Prerequisites

- Go 1.26+
- Make
- Docker
- kubectl
- Kind (optional, for local testing)

### Build the Project

```bash
# Clone the repository
git clone https://github.com/marthydavid/openriak-operator.git
cd openriak-operator

# Download dependencies
go mod download

# Generate CRDs and RBAC
make generate manifests

# Build the operator
make build
```

### Running Tests Locally

```bash
# Run all tests
make test

# Run with coverage
make test-coverage

# Run integration tests (requires Kind)
./hack/test-integration.sh
```

## Project Structure

```
├── api/v1/                     # CRD definitions
│   ├── riakcluster_types.go
│   ├── riakbucket_types.go
│   └── riakuser_types.go
├── internal/
│   ├── controller/             # Reconciliation logic
│   │   ├── riakcluster_controller.go
│   │   ├── riakbucket_controller.go
│   │   └── riakuser_controller.go
│   └── riak/                   # Riak management
│       ├── executor.go         # Shell execution
│       └── manager.go          # High-level operations
├── config/
│   ├── crd/                    # Generated CRDs
│   ├── rbac/                   # RBAC definitions
│   ├── manager/                # Operator deployment
│   └── samples/                # Example resources
├── examples/                   # E2E example manifests
└── hack/                       # Test and build scripts
```

## Making Changes

### Add a New Feature

1. **Create a new branch**
   ```bash
   git checkout -b feature/my-feature
   ```

2. **Make your changes**
   - Update API types in `api/v1/`
   - Implement controller logic in `internal/controller/`
   - Add Riak operations in `internal/riak/`

3. **Generate code**
   ```bash
   make generate manifests
   ```

4. **Write tests**
   - Unit tests in `internal/controller/*_test.go`
   - Add integration tests as needed

5. **Test locally**
   ```bash
   make test
   make docker-build IMG=riak-operator:test
   ```

### Code Standards

- Follow Go conventions (gofmt, golint)
- Add comments for exported functions and types
- Use meaningful variable names
- Keep functions focused and testable

### Commit Messages

Use clear, descriptive commit messages:

```
Feature: Add support for Riak search index management

- Implement RiakSearchIndex CRD
- Add controller reconciliation logic
- Update documentation with examples

Fixes #123
```

### Pull Request Process

1. Ensure all tests pass: `make test`
2. Update documentation as needed
3. Add integration tests for new features
4. Request review from maintainers
5. Address feedback and update PR

## Adding a New CRD

### Example: Adding RiakConfiguration

1. **Create the type file**
   ```bash
   # Edit api/v1/riakconfig_types.go
   ```

2. **Implement the types**
   ```go
   type RiakConfigSpec struct {
       ClusterName string `json:"clusterName"`
       Config map[string]string `json:"config"`
   }

   type RiakConfigStatus struct {
       Phase ConfigPhase `json:"phase"`
       Applied bool `json:"applied"`
   }
   ```

3. **Create the controller**
   ```bash
   # Edit internal/controller/riakconfig_controller.go
   ```

4. **Generate manifests**
   ```bash
   make generate manifests
   ```

5. **Add Riak operations** (if needed)
   ```bash
   # Update internal/riak/manager.go
   ```

6. **Write tests and examples**

## Testing Guidelines

### Unit Tests

```go
func TestRiakClusterReconciliation(t *testing.T) {
    // Arrange
    cluster := &riakv1.RiakCluster{...}
    
    // Act
    result, err := reconciler.Reconcile(ctx, req)
    
    // Assert
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if result.RequeueAfter != expectedDuration {
        t.Errorf("expected requeue after %v, got %v", expectedDuration, result.RequeueAfter)
    }
}
```

### Integration Tests

Run the full integration test:
```bash
./hack/test-integration.sh
```

### Manual Testing

```bash
# Create Kind cluster
kind create cluster --name test

# Load image
make docker-build IMG=riak-operator:test
kind load docker-image riak-operator:test

# Deploy
make install
make deploy IMG=riak-operator:test

# Test with examples
kubectl apply -f examples/1-dev-cluster.yaml
kubectl get riakclusters -w
```

## Documentation

Update documentation when making changes:

1. **README.md**: For user-facing features
2. **.agent.md**: For developer guidance
3. **CONTRIBUTING.md**: For contribution guidelines
4. **Inline comments**: For complex code logic

## Issues and Bugs

### Reporting Bugs

Include:
- Operator version
- Kubernetes version
- Steps to reproduce
- Expected behavior
- Actual behavior
- Relevant logs and events

### Proposing Features

Include:
- Use case and motivation
- Proposed API design
- Implementation approach
- Potential impact on existing features

## Code Review Process

- Maintainers will review for:
  - Code quality and style
  - Test coverage
  - Documentation
  - Performance impact
  - Security implications

- Expected review time: 2-5 business days

## Release Process

Releases follow semantic versioning (MAJOR.MINOR.PATCH):

- Major: Breaking API changes
- Minor: New features (backward compatible)
- Patch: Bug fixes

Release process:
1. Tag commit: `git tag v0.1.0`
2. Push tag: `git push origin v0.1.0`
3. CI automatically creates release

## Getting Help

- Check existing issues
- Review documentation
- Ask in pull request discussion
- Open a new issue with details

## Code of Conduct

- Be respectful and inclusive
- Welcome diverse perspectives
- Address conflicts constructively
- Report harassment to maintainers

## License

All contributions are licensed under the Apache License 2.0.

Thank you for contributing to OpenRiak Operator! 🚀
