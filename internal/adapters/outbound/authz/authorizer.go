package authz

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
)

// DefaultRefreshInterval is the production cadence at which the authorizer
// re-loads the policy from the store. The policy is migration-managed and
// changes rarely, so a coarse interval is fine; tests inject a short one.
const DefaultRefreshInterval = 30 * time.Second

// PolicyLoaderFunc loads the current policy rules from the source of truth. Each
// inner slice is exactly [sub, obj, act], matching the model's `p = sub, obj,
// act`. It is injected (rather than the authorizer calling Postgres directly) so
// the enforce tests can supply a fake and the authorizer stays decoupled from
// pgx — the same seam as US-02.03's VerifierFunc/HasherFunc.
type PolicyLoaderFunc func(ctx context.Context) ([][]string, error)

// CasbinAuthorizer answers authorization questions ("may role R do action A on
// resource O?") from a Casbin enforcer that is periodically refreshed from the
// policy store.
//
// The live enforcer is held in an atomic.Pointer so Enforce is lock-free: it
// loads the current pointer and asks it. Refresh never mutates the live
// enforcer; it builds a BRAND-NEW enforcer from freshly loaded rules and
// atomically swaps the pointer. A concurrent Enforce therefore sees either the
// complete old policy or the complete new one — there is no half-loaded window
// and no duplicate-rule concern.
type CasbinAuthorizer struct {
	loader          PolicyLoaderFunc
	modelText       string
	refreshInterval time.Duration
	logger          *slog.Logger

	// current holds the live *casbin.Enforcer. It is non-nil for the whole life
	// of the value: the constructor stores one before returning, and every
	// successful refresh replaces it with another non-nil enforcer.
	current atomic.Pointer[casbin.Enforcer]

	// done is closed by Close to stop the refresh goroutine; wg lets Close wait
	// for the goroutine to actually exit. closeOnce makes Close idempotent.
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewCasbinAuthorizer parses the model, performs one initial, fail-fast policy
// load, and returns a ready authorizer. The initial load is fail-fast on
// purpose: the process must not start an authorizer that cannot authorize, so a
// loader error or a model/enforcer build failure is returned rather than
// swallowed. An initial load that yields ZERO rules is NOT an error — it builds
// an empty enforcer that denies everything (fail-closed), logging a warning
// because the seed should always be present.
//
// refreshInterval is injectable (DefaultRefreshInterval in production, a short
// value in tests). A nil logger falls back to slog.Default so the background
// refresh goroutine can never panic on a missing logger.
func NewCasbinAuthorizer(
	loader PolicyLoaderFunc,
	modelText string,
	refreshInterval time.Duration,
	logger *slog.Logger,
) (*CasbinAuthorizer, error) {
	if loader == nil {
		return nil, fmt.Errorf("casbin authorizer: loader must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if refreshInterval <= 0 {
		refreshInterval = DefaultRefreshInterval
	}

	a := &CasbinAuthorizer{
		loader:          loader,
		modelText:       modelText,
		refreshInterval: refreshInterval,
		logger:          logger,
		done:            make(chan struct{}),
	}

	// Initial load is fail-fast: propagate any error so the composition root can
	// refuse to start.
	enf, err := a.load(context.Background())
	if err != nil {
		return nil, fmt.Errorf("casbin authorizer initial load: %w", err)
	}
	a.current.Store(enf)
	return a, nil
}

// Enforce reports whether role may perform action on resource. It loads the live
// enforcer (lock-free) and delegates the decision. A non-nil error is an
// enforcer fault (e.g. a malformed matcher), not a deny — callers must
// distinguish "denied" (false, nil) from "could not decide" (_, err) and fail
// closed on the latter.
func (a *CasbinAuthorizer) Enforce(role, resource, action string) (bool, error) {
	enf := a.current.Load()
	allowed, err := enf.Enforce(role, resource, action)
	if err != nil {
		return false, fmt.Errorf("casbin enforce: %w", err)
	}
	return allowed, nil
}

// Start launches the background refresh loop: a ticker at refreshInterval that
// re-loads the policy and atomically swaps in a fresh enforcer. It returns
// immediately; the goroutine runs until ctx is canceled or Close is called.
func (a *CasbinAuthorizer) Start(ctx context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(a.refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.done:
				return
			case <-ticker.C:
				a.refresh(ctx)
			}
		}
	}()
}

// Close stops the refresh goroutine and waits for it to exit. It is idempotent
// and safe to call even if Start was never called (wg.Wait returns at once).
func (a *CasbinAuthorizer) Close() {
	a.closeOnce.Do(func() { close(a.done) })
	a.wg.Wait()
}

// refresh re-loads the policy and atomically swaps in a fresh enforcer. A load
// that FAILS (loader error or build error) is logged and the last good enforcer
// is kept in place — a transient store outage must not blow the policy away. A
// load that SUCCEEDS with zero rules is treated as fail-closed (the empty
// enforcer denies everything) and swapped in, with a warning, because the seed
// should always be present.
func (a *CasbinAuthorizer) refresh(ctx context.Context) {
	enf, err := a.load(ctx)
	if err != nil {
		a.logger.LogAttrs(ctx, slog.LevelError,
			"authz: policy refresh failed; keeping last good policy",
			slog.String("error", err.Error()))
		return
	}
	a.current.Store(enf)
}

// load fetches the current rules and builds a fresh enforcer from them. It warns
// (but does not error) on an empty rule set so the deny-all fail-closed default
// is visible in the logs. It is the single place both the initial load and every
// refresh go through, so the empty-warning and build logic live in one spot.
func (a *CasbinAuthorizer) load(ctx context.Context) (*casbin.Enforcer, error) {
	rules, err := a.loader(ctx)
	if err != nil {
		return nil, fmt.Errorf("load policy rules: %w", err)
	}
	if len(rules) == 0 {
		a.logger.LogAttrs(ctx, slog.LevelWarn,
			"authz: policy load returned zero rules; enforcer will deny all (fail-closed)")
	}
	return a.buildEnforcer(rules)
}

// buildEnforcer parses the model afresh and loads rules into a new enforcer. A
// fresh model per build is deliberate: the model carries the in-memory policy,
// so reusing one across enforcers would accumulate rules. casbin.NewEnforcer is
// called with the model only (no persistence adapter); AddPolicies with a nil
// adapter is safe — it skips persistence (guarded by shouldPersist) and updates
// only the in-memory model.
func (a *CasbinAuthorizer) buildEnforcer(rules [][]string) (*casbin.Enforcer, error) {
	m, err := model.NewModelFromString(a.modelText)
	if err != nil {
		return nil, fmt.Errorf("parse casbin model: %w", err)
	}
	enf, err := casbin.NewEnforcer(m)
	if err != nil {
		return nil, fmt.Errorf("create casbin enforcer: %w", err)
	}
	if len(rules) > 0 {
		if _, err := enf.AddPolicies(rules); err != nil {
			return nil, fmt.Errorf("load casbin policies: %w", err)
		}
	}
	return enf, nil
}
