package edge

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

const edgeRequestPolicyGuardKey = "edge_request_policy_guard"

var (
	edgeAdmission       = newEdgeAdmissionGate()
	edgeDataPlanePolicy sync.RWMutex
	edgeAccountingReady atomic.Bool
	edgeAccountingBlock atomic.Bool
	edgeBalanceReady    atomic.Bool
)

func init() {
	edgeAccountingReady.Store(true)
}

type edgeAdmissionGateState struct {
	mu        sync.Mutex
	accepting bool
	inFlight  int
	zero      chan struct{}
}

type edgeRequestPolicyGuard struct {
	policyOnce sync.Once
	endOnce    sync.Once
}

func (g *edgeRequestPolicyGuard) release() {
	if g == nil {
		return
	}
	g.policyOnce.Do(edgeDataPlanePolicy.RUnlock)
}

func newEdgeAdmissionGate() *edgeAdmissionGateState {
	zero := make(chan struct{})
	close(zero)
	return &edgeAdmissionGateState{zero: zero}
}

// SetEdgeRequestAdmission controls whether new data-plane handlers may enter.
// Snapshot validity is checked separately by EdgeControlReady on every entry.
func SetEdgeRequestAdmission(accepting bool) {
	edgeAdmission.mu.Lock()
	edgeAdmission.accepting = accepting
	edgeAdmission.mu.Unlock()
}

func EdgeServingReady() bool {
	edgeAdmission.mu.Lock()
	accepting := edgeAdmission.accepting
	edgeAdmission.mu.Unlock()
	return accepting && EdgeControlReady() && edgeBalanceReady.Load() && edgeAccountingReady.Load() && !edgeAccountingBlock.Load()
}

func BeginEdgeRequest(c *gin.Context) bool {
	if c == nil {
		return false
	}
	edgeDataPlanePolicy.RLock()
	edgeAdmission.mu.Lock()
	defer edgeAdmission.mu.Unlock()
	if !edgeAdmission.accepting || !EdgeControlReady() || !edgeBalanceReady.Load() || !edgeAccountingReady.Load() || edgeAccountingBlock.Load() {
		edgeDataPlanePolicy.RUnlock()
		return false
	}
	if edgeAdmission.inFlight == 0 {
		edgeAdmission.zero = make(chan struct{})
	}
	edgeAdmission.inFlight++
	c.Set(edgeRequestPolicyGuardKey, &edgeRequestPolicyGuard{})
	return true
}

// ReleaseEdgeRequestPolicy releases the request's snapshot/runtime read guard
// once local lease reservation has pinned the exact signed pricing snapshot.
// The handler remains counted as in-flight until EndEdgeRequest runs.
func ReleaseEdgeRequestPolicy(c *gin.Context) {
	if c == nil {
		return
	}
	value, exists := c.Get(edgeRequestPolicyGuardKey)
	if !exists {
		return
	}
	guard, ok := value.(*edgeRequestPolicyGuard)
	if ok {
		guard.release()
	}
}

func EndEdgeRequest(c *gin.Context) {
	if c == nil {
		return
	}
	value, exists := c.Get(edgeRequestPolicyGuardKey)
	if !exists {
		return
	}
	guard, ok := value.(*edgeRequestPolicyGuard)
	if !ok {
		return
	}
	guard.release()
	guard.endOnce.Do(func() {
		edgeAdmission.mu.Lock()
		defer edgeAdmission.mu.Unlock()
		if edgeAdmission.inFlight <= 0 {
			return
		}
		edgeAdmission.inFlight--
		if edgeAdmission.inFlight == 0 {
			close(edgeAdmission.zero)
		}
	})
}

// withEdgeDataPlanePolicyMutation prevents a snapshot or CPA runtime refresh
// from interleaving with local authentication, channel selection, pricing and
// lease reservation. Long-running CPA relay work does not hold this lock.
func withEdgeDataPlanePolicyMutation(mutate func() error) error {
	edgeDataPlanePolicy.Lock()
	defer edgeDataPlanePolicy.Unlock()
	return mutate()
}

// WithEdgeDataPlanePolicyRead keeps one short retry-selection operation on the
// same snapshot/runtime revision. Initial request processing uses the longer
// guard installed by BeginEdgeRequest and releases it after lease reservation.
func WithEdgeDataPlanePolicyRead(read func() error) error {
	edgeDataPlanePolicy.RLock()
	defer edgeDataPlanePolicy.RUnlock()
	return read()
}

func WaitForEdgeRequests(ctx context.Context) error {
	edgeAdmission.mu.Lock()
	zero := edgeAdmission.zero
	edgeAdmission.mu.Unlock()
	select {
	case <-zero:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
