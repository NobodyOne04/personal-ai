// Package deploymentapp holds the deployment use cases: create, launch, stop,
// restart, delete, status, list, health, pinning, model-to-deployment routing
// helpers and startup reconciliation.
//
// Every status change goes through an optimistic-locking update loop, so a
// background launch and a concurrent stop can never overwrite each other: the
// loser sees the other's change and backs off. A deployment is never dropped
// while it holds compute.
package deploymentapp
