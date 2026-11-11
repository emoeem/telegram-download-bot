//go:build !android

package license

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 服务端账本的语义错误。
var (
	// ErrKeyUnknown 表示这把密钥从未签发给任何人。服务端只接受自己签发过的
	// 密钥，不认识的密钥一律拒绝——否则攻击者可以自己造一把"永远有效"的密钥。
	ErrKeyUnknown = errors.New("密钥未在本服务器签发记录中")

	// ErrKeyBoundElsewhere 表示这把密钥已经绑在另一台设备上。
	// 这是「一密钥一设备」的唯一执行点：客户端改文件绕不过它。
	ErrKeyBoundElsewhere = errors.New("密钥已绑定到其他设备")

	// ErrKeyRevoked 表示这把密钥已被撤销。
	ErrKeyRevoked = errors.New("密钥已被撤销")

	// ErrKeyExpired 表示密钥有效期已过。
	ErrKeyExpired = errors.New("密钥已过期")

	// ErrDeviceMissing 表示请求没带设备码。
	ErrDeviceMissing = errors.New("缺少设备码")
)

// RegistryEntry 是服务端为一把密钥保存的授权记录。
//
// 服务端账本是「一密钥一设备」的唯一权威：客户端本地的 activation.json 只是
// 缓存，改它不会改变服务端的绑定关系；服务端说这把密钥属于哪台设备，就是哪台。
type RegistryEntry struct {
	// Key 是完整密钥文本。存在服务端，客户端侧不落盘明文（只留指纹）。
	Key string `json:"key"`

	// Device 是绑定的设备码，8 位 Crockford Base32。
	Device string `json:"device"`

	// Kind 是密钥种类（short / strong / semi）。
	Kind string `json:"kind"`

	// IssuedAt 是签发时间，ExpiryDay 是有效天数（0 表示永久）。
	IssuedAt  time.Time `json:"issued_at"`
	ExpiryDay uint16    `json:"expiry_day"`
	Forever   bool      `json:"forever"`

	// BoundAt 是首次激活时间。绑上之后不再改变——续期也不改，因为换机会被拒。
	BoundAt time.Time `json:"bound_at"`

	// LastSeen 是最后一次心跳时间。用来识别长期不用的授权（可回收）。
	LastSeen time.Time `json:"last_seen"`

	// Revoked 为真时，客户端下次心跳即失效。
	Revoked   bool       `json:"revoked"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`

	// Note 供运营备注（客户名、订单号）。
	Note string `json:"note"`

	// Refusals 记录被拒绝的换机尝试次数。这个数字是盗用信号：
	// 正常客户不会反复用同一把密钥去激活第三台设备。
	Refusals int `json:"refusals"`
}

// ExpiresAt 返回绝对到期时间。永久密钥返回零值。
func (e RegistryEntry) ExpiresAt() time.Time {
	if e.Forever || e.ExpiryDay == 0 {
		return time.Time{}
	}
	return e.IssuedAt.Add(time.Duration(e.ExpiryDay) * 24 * time.Hour)
}

// Expired 判断密钥在 now 是否已过期。
func (e RegistryEntry) Expired(now time.Time) bool {
	if e.Forever || e.ExpiryDay == 0 {
		return false
	}
	return !now.Before(e.ExpiresAt())
}

// DaysLeft 返回剩余天数；永久密钥返回 -1。
func (e RegistryEntry) DaysLeft(now time.Time) int {
	if e.Forever || e.ExpiryDay == 0 {
		return -1
	}
	d := int(e.ExpiresAt().Sub(now).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

// KeyFingerprint 返回密钥的短指纹（前 4 段），用于日志与界面。
// 密钥全文不进日志：一份包含所有客户密钥的日志文件，等于把这些密钥都泄露了。
func KeyFingerprint(key string) string {
	if len(key) <= 9 {
		return "***"
	}
	if len(key) > 24 {
		return key[:24] + "…"
	}
	return key
}

// Registry 是落盘的密钥账本。
//
// 单文件 JSON + 互斥锁 + 原子写（tmp + rename）。这个规模（几百到几千条）不需要
// 数据库，而一个 JSON 文件可以被直接备份、压缩、拷走，运维成本最低。
type Registry struct {
	mu   sync.Mutex
	path string
	data map[string]*RegistryEntry
}

// OpenRegistry 打开（或创建）位于 path 的账本。
func OpenRegistry(path string) (*Registry, error) {
	if path == "" {
		return nil, errors.New("账本路径为空")
	}
	r := &Registry{path: path, data: map[string]*RegistryEntry{}}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var payload struct {
			Version int                       `json:"version"`
			Keys    map[string]*RegistryEntry `json:"keys"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, fmt.Errorf("账本 %s 解析失败: %w", path, err)
		}
		for k, v := range payload.Keys {
			if v != nil {
				r.data[k] = v
			}
		}
	case os.IsNotExist(err):
		// 首次运行：空账本，第一次写入时落盘。
	default:
		return nil, fmt.Errorf("读取账本 %s 失败: %w", path, err)
	}
	return r, nil
}

// Path 返回账本文件路径。
func (r *Registry) Path() string { return r.path }

// saveLocked 原子落盘。调用方必须已持有 r.mu。
func (r *Registry) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	payload := struct {
		Version int                       `json:"version"`
		Updated time.Time                 `json:"updated"`
		Keys    map[string]*RegistryEntry `json:"keys"`
	}{Version: 1, Updated: time.Now().UTC(), Keys: r.data}

	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	// 账本里有客户密钥全文，权限必须收紧到 0600。
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// Activate 把 key 绑定到 device。
//
// 规则，按顺序：
//  1. 密钥必须已在账本中（服务端签发过）；
//  2. 密钥未撤销、未过期；
//  3. 若已绑定：设备必须相同，否则拒绝并记一次 Refusals。
//
// 返回的 entry 是绑定后的最新状态。
func (r *Registry) Activate(key, device string, now time.Time) (RegistryEntry, error) {
	if device == "" {
		return RegistryEntry{}, ErrDeviceMissing
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.data[key]
	if !ok {
		return RegistryEntry{}, ErrKeyUnknown
	}
	if e.Revoked {
		return RegistryEntry{}, ErrKeyRevoked
	}
	if e.Expired(now) {
		return RegistryEntry{}, ErrKeyExpired
	}

	if e.Device == "" {
		// 首次激活：占位。
		e.Device = device
		e.BoundAt = now
		e.LastSeen = now
		if err := r.saveLocked(); err != nil {
			return RegistryEntry{}, err
		}
		return *e, nil
	}

	if e.Device != device {
		// 换机尝试。记数后拒绝——这个计数就是盗用证据。
		e.Refusals++
		_ = r.saveLocked()
		return *e, ErrKeyBoundElsewhere
	}

	e.LastSeen = now
	if err := r.saveLocked(); err != nil {
		return RegistryEntry{}, err
	}
	return *e, nil
}

// Verify 是心跳校验：不改变绑定关系，只确认这把密钥此刻对这台设备仍然有效。
func (r *Registry) Verify(key, device string, now time.Time) (RegistryEntry, error) {
	if device == "" {
		return RegistryEntry{}, ErrDeviceMissing
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.data[key]
	if !ok {
		return RegistryEntry{}, ErrKeyUnknown
	}
	if e.Revoked {
		return RegistryEntry{}, ErrKeyRevoked
	}
	if e.Expired(now) {
		return RegistryEntry{}, ErrKeyExpired
	}
	if e.Device != "" && e.Device != device {
		return RegistryEntry{}, ErrKeyBoundElsewhere
	}

	e.LastSeen = now
	if err := r.saveLocked(); err != nil {
		return RegistryEntry{}, err
	}
	return *e, nil
}

// Admit 登记一把刚签发出去的密钥，使其可以被激活。
//
// 签发与登记是两步：签发只需要主密钥（可以离线、可以在另一台机器上），
// 登记需要账本。这样"卖密钥的人"和"跑服务器的人"可以分开。
func (r *Registry) Admit(key, kind string, issuedAt time.Time, expiryDay uint16, note string) (RegistryEntry, error) {
	if key == "" {
		return RegistryEntry{}, errors.New("密钥为空")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.data[key]; ok {
		// 幂等：重复登记不覆盖已有绑定。
		return *e, nil
	}
	e := &RegistryEntry{
		Key:       key,
		Kind:      kind,
		IssuedAt:  issuedAt,
		ExpiryDay: expiryDay,
		Forever:   expiryDay == 0,
		Note:      note,
	}
	r.data[key] = e
	if err := r.saveLocked(); err != nil {
		return RegistryEntry{}, err
	}
	return *e, nil
}

// Revoke 撤销一把密钥。撤销后客户端心跳即失效，且不可再激活。
func (r *Registry) Revoke(key string, now time.Time) (RegistryEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.data[key]
	if !ok {
		return RegistryEntry{}, ErrKeyUnknown
	}
	if !e.Revoked {
		e.Revoked = true
		at := now
		e.RevokedAt = &at
		if err := r.saveLocked(); err != nil {
			return RegistryEntry{}, err
		}
	}
	return *e, nil
}

// Release 解绑（客服换机场景：客户换手机了，运营把旧绑定清掉）。
// 这不是撤销：密钥仍然有效，下一次激活可以绑到新设备。
func (r *Registry) Release(key string) (RegistryEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.data[key]
	if !ok {
		return RegistryEntry{}, ErrKeyUnknown
	}
	e.Device = ""
	e.BoundAt = time.Time{}
	if err := r.saveLocked(); err != nil {
		return RegistryEntry{}, err
	}
	return *e, nil
}

// List 返回全部记录，按签发时间倒序。
func (r *Registry) List() []RegistryEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]RegistryEntry, 0, len(r.data))
	for _, e := range r.data {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IssuedAt.After(out[j].IssuedAt) })
	return out
}

// Get 按密钥查一条记录。
func (r *Registry) Get(key string) (RegistryEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.data[key]
	if !ok {
		return RegistryEntry{}, false
	}
	return *e, true
}

// Count 返回账本中的密钥总数。
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.data)
}
