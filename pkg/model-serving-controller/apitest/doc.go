/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package apitest provides an in-memory stand-in for the Kubernetes API server
// for controller unit tests.
//
// A Cluster wires three fake clientsets (core Kubernetes, kthena and Volcano)
// to one shared store so that a controller can be exercised against a single
// API surface. On top of the stock client-go fake clientsets it adds the
// API-server guarantees controllers depend on:
//
//   - optimistic concurrency: Update, UpdateStatus and merge patches that carry
//     a stale metadata.resourceVersion (or a foreign metadata.uid) are rejected
//     with a Conflict error, and so are deletes whose DeleteOptions
//     preconditions do not match;
//   - metadata stamping: created objects get a UID, a creationTimestamp and
//     generation 1, and every accepted write bumps a cluster-wide monotonic
//     resourceVersion;
//   - status subresource semantics: a write through the main resource never
//     changes status, a write through status changes nothing but status, and
//     metadata.generation only moves when spec changes;
//   - finalizers and garbage collection: foreground, background and orphan
//     deletion propagation are modelled explicitly, driven by CollectGarbage
//     and Settle so that tests stay deterministic and free of goroutines;
//   - observability: Writes returns the ordered log of accepted mutations and
//     BeforeWrite can reject a chosen write to simulate an API failure.
//
// Snapshot hands out informer-style stores so a controller can run against a
// consistent (or deliberately stale) cache without starting informers.
package apitest
