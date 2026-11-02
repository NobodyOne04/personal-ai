// Package deploymentapp will hold the deployment use cases: create, start,
// stop, restart, delete, status, list, health, pinning, model-to-deployment
// routing, inference orchestration and startup reconciliation.
//
// Lifecycle orchestration is implemented in Phase 5 and routing in Phase 6.
// The package exists now so that the dependency rules are enforced from the
// first commit.
package deploymentapp
