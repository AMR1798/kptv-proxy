package proxy

import (
	"hash/fnv"
	"kptv-proxy/work/config"
	"sync"
	"sync/atomic"
)

type xcAccountState struct {
	account    config.XCOutputAccount
	generation uint64
	active     atomic.Int32
}

// XCAccount is an immutable authenticated account view. Its state remains
// alive after a reload so sessions admitted before deletion can release safely.
type XCAccount struct {
	Config     config.XCOutputAccount
	Generation uint64
	state      *xcAccountState
}

func (a *XCAccount) ActiveConnections() int32 { return a.state.active.Load() }

func (a *XCAccount) TryAcquirePlayback() (*XCPlaybackLease, bool) {
	for {
		current := a.state.active.Load()
		if current >= int32(a.state.account.MaxConnections) {
			return nil, false
		}
		if a.state.active.CompareAndSwap(current, current+1) {
			return &XCPlaybackLease{state: a.state}, true
		}
	}
}

func (a *XCAccount) Allows(contentType string) bool {
	switch contentType {
	case "live":
		return a.Config.EnableLive
	case "vod":
		return a.Config.EnableVOD
	case "series", "episode":
		return a.Config.EnableSeries
	default:
		return false
	}
}

type XCPlaybackLease struct {
	state    *xcAccountState
	released atomic.Bool
}

func (l *XCPlaybackLease) Release() {
	if l != nil && l.released.CompareAndSwap(false, true) {
		l.state.active.Add(-1)
	}
}

type XCAccountRegistry struct {
	mu          sync.RWMutex
	current     map[int64]*xcAccountState
	credentials map[string]*xcAccountState
	generation  atomic.Uint64
}

func NewXCAccountRegistry(accounts []config.XCOutputAccount) *XCAccountRegistry {
	r := &XCAccountRegistry{}
	r.Replace(accounts)
	return r
}

func (r *XCAccountRegistry) Replace(accounts []config.XCOutputAccount) {
	r.mu.Lock()
	defer r.mu.Unlock()
	generation := r.generation.Add(1)
	current := make(map[int64]*xcAccountState, len(accounts))
	credentials := make(map[string]*xcAccountState, len(accounts))
	for i, account := range accounts {
		id := account.ID
		if id == 0 {
			id = syntheticXCAccountID(account, i)
		}
		state := r.current[id]
		if state == nil {
			state = &xcAccountState{}
		}
		state.account = account
		state.account.ID = id
		state.generation = generation
		current[id] = state
		credentials[account.Username+"\x00"+account.Password] = state
	}
	r.current = current
	r.credentials = credentials
}

func syntheticXCAccountID(account config.XCOutputAccount, index int) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(account.Username + "\x00" + account.Password + "\x00" + account.Name))
	return -int64(h.Sum64()&0x7FFFFFFFFFFFFFFF) - int64(index)
}

func (r *XCAccountRegistry) Authenticate(username, password string) (*XCAccount, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.credentials[username+"\x00"+password]
	if state == nil {
		return nil, false
	}
	return &XCAccount{Config: state.account, Generation: state.generation, state: state}, true
}

func (r *XCAccountRegistry) ActiveConnections(id int64) int32 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if state := r.current[id]; state != nil {
		return state.active.Load()
	}
	return 0
}

func (r *XCAccountRegistry) Acquire(account *XCAccount, contentType string) (*XCPlaybackLease, bool) {
	if account == nil || !account.Allows(contentType) {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	current := r.current[account.Config.ID]
	valid := current == account.state && current.generation == account.Generation
	if !valid {
		return nil, false
	}
	return account.TryAcquirePlayback()
}

func (r *XCAccountRegistry) IsCurrent(account *XCAccount) bool {
	if account == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.current[account.Config.ID]
	return state == account.state && state.generation == account.Generation
}
