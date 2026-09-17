// fetch is the direct form of fetchGen, kept for tests that drive one refresh
// without the generation bookkeeping.

package netinfo

import "context"

// fetch gathers the fast connection fields under a fresh generation. It backs
// direct callers (tests); the refresh path calls fetchGen with the generation it
// already claimed so the miss-state gate lines up with its later publishes.
func (m *Manager) fetch(ctx context.Context) Info { return m.fetchGen(ctx, m.nextGen()) }
