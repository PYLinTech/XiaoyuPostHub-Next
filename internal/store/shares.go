package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const shareColumns = `id, owner_id, root_path, kind, access_mode, pwd_hash, pwd_salt,
	allow_download, allow_preview, allow_subpath, show_sharer_name, expires_at, max_visits, visits,
	disabled, created_at, updated_at`

func scanShare(row rowScanner) (Share, error) {
	var s Share
	var kind, mode string
	var allowDown, allowPrev, allowSub, showName, disabled int
	err := row.Scan(&s.ID, &s.OwnerID, &s.RootPath, &kind, &mode, &s.PwdHash, &s.PwdSalt,
		&allowDown, &allowPrev, &allowSub, &showName, &s.ExpiresAt, &s.MaxVisits, &s.Visits,
		&disabled, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return Share{}, err
	}
	s.Kind = ShareKind(kind)
	s.AccessMode = AccessMode(mode)
	s.AllowDownload = allowDown != 0
	s.AllowPreview = allowPrev != 0
	s.AllowSubpath = allowSub != 0
	s.ShowSharerName = showName != 0
	s.Disabled = disabled != 0
	s.HasPassword = s.PwdHash != ""
	return s, nil
}

// CreateShare 新建分享。
//
// id 是随机不可枚举的 token：可枚举的短 id 等于把"遍历全部分享"变成一次
// 简单的循环。
func CreateShare(ctx context.Context, q Querier, s Share) error {
	if s.ID == "" {
		id, err := GenerateToken(16)
		if err != nil {
			return err
		}
		s.ID = id
	}
	if s.AccessMode == "" {
		s.AccessMode = AccessPublic
	}
	if s.Kind == "" {
		s.Kind = ShareFile
	}
	// updated_at 由调用方给初值而不是这里盖时间戳：CreateShare 返回的是调用方
	// 手里那个结构体，它必须与落库版本一致，否则紧接着的一次 CAS 更新会拿着
	// 一个库里根本不存在的版本号自撞冲突。
	_, err := q.ExecContext(ctx, `
		INSERT INTO shares (id, owner_id, root_path, kind, access_mode, pwd_hash, pwd_salt,
			allow_download, allow_preview, allow_subpath, show_sharer_name, expires_at, max_visits, visits,
			disabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		s.ID, s.OwnerID, s.RootPath, string(s.Kind), string(s.AccessMode), s.PwdHash, s.PwdSalt,
		boolToInt(s.AllowDownload), boolToInt(s.AllowPreview), boolToInt(s.AllowSubpath), boolToInt(s.ShowSharerName),
		s.ExpiresAt, s.MaxVisits, Now(), s.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 分享标识冲突", ErrConflict)
		}
		return fmt.Errorf("创建分享失败: %w", err)
	}
	return nil
}

// GetShare 按标识取分享。
func GetShare(ctx context.Context, q Querier, id string) (Share, error) {
	row := q.QueryRowContext(ctx, `SELECT `+shareColumns+` FROM shares WHERE id = ?`, id)
	s, err := scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Share{}, ErrNotFound
	}
	if err != nil {
		return Share{}, fmt.Errorf("读取分享失败: %w", err)
	}
	return s, nil
}

// ListSharesByOwner 列出某用户创建的分享。
func ListSharesByOwner(ctx context.Context, q Querier, ownerID int64, limit, offset int) ([]Share, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `SELECT `+shareColumns+`
		FROM shares WHERE owner_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		ownerID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("列出分享失败: %w", err)
	}
	defer rows.Close()
	var out []Share
	for rows.Next() {
		s, err := scanShare(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描分享失败: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateShareIfUnchanged 按乐观锁更新分享。
//
// 谓词 updated_at = expectUpdatedAt 保证写入基于调用方读到的同一版本：
// 并发期间已有别人提交，本次陈旧写回命中 0 行。0 行需区分"分享不存在"
// 与"版本冲突"——再做一次存在性检查映射成对应错误。
func UpdateShareIfUnchanged(ctx context.Context, q Querier, s Share, expectUpdatedAt int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE shares SET root_path = ?, kind = ?, access_mode = ?, pwd_hash = ?, pwd_salt = ?,
			allow_download = ?, allow_preview = ?, allow_subpath = ?, show_sharer_name = ?,
			expires_at = ?, max_visits = ?, disabled = ?, updated_at = updated_at + 1
		WHERE id = ? AND updated_at = ?`,
		s.RootPath, string(s.Kind), string(s.AccessMode), s.PwdHash, s.PwdSalt,
		boolToInt(s.AllowDownload), boolToInt(s.AllowPreview), boolToInt(s.AllowSubpath), boolToInt(s.ShowSharerName),
		s.ExpiresAt, s.MaxVisits, boolToInt(s.Disabled), s.ID, expectUpdatedAt)
	if err != nil {
		return fmt.Errorf("更新分享失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		return nil
	}
	if _, getErr := GetShare(ctx, q, s.ID); getErr == nil {
		return fmt.Errorf("%w: 分享已被其他人修改，请刷新后重试", ErrConflict)
	}
	return ErrNotFound
}

// DeleteShare 删除分享（取件码与访问记录随之级联删除）。
func DeleteShare(ctx context.Context, q Querier, id string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM shares WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除分享失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ConsumeShareVisit 记一次访问。
//
// 有效性、过期、访问上限三者与计数递增放在同一条 UPDATE：分开判会并发超限。
// 命中 0 行即表示拒绝。
func ConsumeShareVisit(ctx context.Context, q Querier, id string, now int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE shares SET visits = visits + 1
		WHERE id = ? AND disabled = 0
		  AND (expires_at = 0 OR expires_at > ?)
		  AND (max_visits = 0 OR visits < max_visits)
		  AND EXISTS (
			SELECT 1 FROM users
			 WHERE users.id = shares.owner_id
			   AND users.status = 1
		  )`, id, now)
	if err != nil {
		return fmt.Errorf("记录分享访问失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: 分享已失效、已过期或已达访问上限", ErrNoRowsAffected)
	}
	return nil
}

// ShareRoot 是票据吊销所需的最小分享信息。
type ShareRoot struct {
	ID       string
	OwnerID  int64
	RootPath string
}

// ListSharesExpiredSince 列出在 (since, now] 区间内自然过期的分享，
// 供维护任务吊销其在途票据。
//
// 窗口扫描而非全量扫描：票据自身有效期只有票据 TTL 量级，窗口覆盖两个
// 维护周期即可保证不漏吊销，同时避免扫过全部历史过期分享。
func ListSharesExpiredSince(ctx context.Context, q Querier, now, since int64, limit int) ([]ShareRoot, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, owner_id, root_path FROM shares
		WHERE disabled = 0 AND expires_at > ? AND expires_at <= ?
		ORDER BY expires_at LIMIT ?`, since, now, limit)
	if err != nil {
		return nil, fmt.Errorf("查询过期分享失败: %w", err)
	}
	defer rows.Close()
	var out []ShareRoot
	for rows.Next() {
		var s ShareRoot
		if err := rows.Scan(&s.ID, &s.OwnerID, &s.RootPath); err != nil {
			return nil, fmt.Errorf("扫描过期分享失败: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DisableSharesByRootPrefix 让覆盖某路径的分享全部失效。
func DisableSharesByRootPrefix(ctx context.Context, q Querier, ownerID int64, path string) (int64, error) {
	clause, args := pathSelfOrDescendant("root_path", path)
	res, err := q.ExecContext(ctx, `
		UPDATE shares SET disabled = 1
		WHERE owner_id = ? AND disabled = 0 AND `+clause,
		append([]any{ownerID}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("失效分享失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// InsertShareAccess 记录一次分享访问。
func InsertShareAccess(ctx context.Context, q Querier, shareID string, actor ActorType, userID int64, ip, action string) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO share_accesses (share_id, actor_type, user_id, client_ip, action, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?)`, shareID, string(actor), userID, ip, action, Now())
	if err != nil {
		return fmt.Errorf("记录分享访问明细失败: %w", err)
	}
	return nil
}

// ListShareAccesses 列出某分享的访问明细。
func ListShareAccesses(ctx context.Context, q Querier, shareID string, limit int) ([]AuditLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, actor_type, user_id, client_ip, action, occurred_at
		FROM share_accesses WHERE share_id = ? ORDER BY occurred_at DESC LIMIT ?`, shareID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询分享访问明细失败: %w", err)
	}
	defer rows.Close()
	var out []AuditLog
	for rows.Next() {
		var l AuditLog
		if err := rows.Scan(&l.ID, &l.ActorType, &l.ActorID, &l.ClientIP, &l.Action, &l.OccurredAt); err != nil {
			return nil, fmt.Errorf("扫描分享访问明细失败: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- 取件码

const pickupColumns = `code, share_id, max_uses, used_count, disabled, created_by, created_at`

func scanPickup(row rowScanner) (PickupCode, error) {
	var p PickupCode
	var disabled int
	err := row.Scan(&p.Code, &p.ShareID, &p.MaxUses, &p.UsedCount,
		&disabled, &p.CreatedBy, &p.CreatedAt)
	if err != nil {
		return PickupCode{}, err
	}
	p.Disabled = disabled != 0
	return p, nil
}

// CreatePickupCode 新建取件码。
//
// 取件码是分享的短码别名而不是第二套语义：提取码校验、有效期、权限位、
// 访问计数、路径穿越防护全部复用 shares 的实现，避免两套逻辑行为不一致。
// 取件码不记录单独的到期时间：有效期是管理员统一配置，按 created_at 动态判定。
func CreatePickupCode(ctx context.Context, q Querier, p PickupCode) error {
	if p.Code == "" {
		return fmt.Errorf("取件码不能为空")
	}
	if p.MaxUses <= 0 {
		return fmt.Errorf("取件码使用次数必须大于 0")
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO pickup_codes (code, share_id, max_uses, used_count, disabled, created_by, created_at)
		VALUES (?, ?, ?, 0, 0, ?, ?)`,
		p.Code, p.ShareID, p.MaxUses, p.CreatedBy, Now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 取件码 %s 已被占用", ErrConflict, p.Code)
		}
		return fmt.Errorf("创建取件码失败: %w", err)
	}
	return nil
}

// GetPickupCode 按码取取件码。
func GetPickupCode(ctx context.Context, q Querier, code string) (PickupCode, error) {
	row := q.QueryRowContext(ctx, `SELECT `+pickupColumns+` FROM pickup_codes WHERE code = ?`, code)
	p, err := scanPickup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PickupCode{}, ErrNotFound
	}
	if err != nil {
		return PickupCode{}, fmt.Errorf("读取取件码失败: %w", err)
	}
	return p, nil
}

// ConsumePickupCode 原子消费一次取件码，返回其指向的分享标识。
//
// aliveAfter 是有效期窗口下界（now - 全局配置有效期；永久时传 0）：
// created_at > aliveAfter 保证已被管理员缩短有效期而"超龄"的码在 SQL 层
// 就无法核销，判定与计数同一条 UPDATE 完成，并发下不会超发。
func ConsumePickupCode(ctx context.Context, q Querier, code string, aliveAfter int64) (string, error) {
	p, err := GetPickupCode(ctx, q, code)
	if err != nil {
		return "", err
	}
	res, err := q.ExecContext(ctx, `
		UPDATE pickup_codes SET used_count = used_count + 1
		WHERE code = ? AND disabled = 0 AND used_count < max_uses AND created_at > ?`,
		code, aliveAfter)
	if err != nil {
		return "", fmt.Errorf("消费取件码失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("%w: 取件码无效、已用尽或已过期", ErrNoRowsAffected)
	}
	return p.ShareID, nil
}

// CountAlivePickupCodes 返回全局仍在有效期窗口内、未停用的取件码数量。
//
// 这是码池占用的口径：这些行的码值正被占用，新码不能与之相同。
// aliveAfter 为有效期窗口下界，永久（有效期配置为 0）时传 0。
func CountAlivePickupCodes(ctx context.Context, q Querier, aliveAfter int64) (int64, error) {
	var count int64
	if err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pickup_codes WHERE disabled = 0 AND created_at > ?`,
		aliveAfter).Scan(&count); err != nil {
		return 0, fmt.Errorf("统计存活取件码失败: %w", err)
	}
	return count, nil
}

// ListPickupCodesByShare 列出某个分享下的全部取件码。
func ListPickupCodesByShare(ctx context.Context, q Querier, shareID string) ([]PickupCode, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+pickupColumns+`
		FROM pickup_codes WHERE share_id = ? ORDER BY created_at DESC`, shareID)
	if err != nil {
		return nil, fmt.Errorf("列出取件码失败: %w", err)
	}
	defer rows.Close()
	var out []PickupCode
	for rows.Next() {
		p, err := scanPickup(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描取件码失败: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPickupDisabled 启用或停用取件码。
func SetPickupDisabled(ctx context.Context, q Querier, code string, disabled bool) error {
	res, err := q.ExecContext(ctx,
		`UPDATE pickup_codes SET disabled = ? WHERE code = ?`, boolToInt(disabled), code)
	if err != nil {
		return fmt.Errorf("变更取件码状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePickupCode 删除取件码。
func DeletePickupCode(ctx context.Context, q Querier, code string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM pickup_codes WHERE code = ?`, code)
	if err != nil {
		return fmt.Errorf("删除取件码失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
