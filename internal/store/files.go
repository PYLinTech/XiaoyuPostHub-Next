package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const fileColumns = `checksum, size_plain, pan_file_id, pan_object_name, pan_size_wire,
	enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id,
	status, disable_reason, disabled_by, disabled_at, ref_count, created_by, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFile(row rowScanner) (File, error) {
	var f File
	var status int
	err := row.Scan(
		&f.Checksum, &f.SizePlain, &f.PanFileID, &f.PanObjectName, &f.PanSizeWire,
		&f.EncAlgo, &f.EncChunkLog2, &f.EncNoncePrefix, &f.EncSalt, &f.DEKEnvelope, &f.KEKKeyID,
		&status, &f.DisableReason, &f.DisabledBy, &f.DisabledAt, &f.RefCount, &f.CreatedBy,
		&f.CreatedAt, &f.UpdatedAt,
	)
	if err != nil {
		return File{}, err
	}
	f.Status = FileStatus(status)
	return f, nil
}

// InsertFilePlaceholder 以"上传中"状态占位。
//
// 内容池主键本身就是并发上传的锁：返回 created=false 表示同校验码的对象
// 已有人在上传或已存在，调用方应改为等待或复用，而不是重复上传。
//
// 加密参数（盐、nonce 前缀、DEK 信封）必须在这里确定：整个上传过程复用
// 同一组参数，中断重传后的密文才与首次一致。
func InsertFilePlaceholder(ctx context.Context, q Querier, f File) (bool, error) {
	res, err := q.ExecContext(ctx, `
		INSERT OR IGNORE INTO files (
			checksum, size_plain, pan_file_id, pan_object_name, pan_size_wire,
			enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id,
			status, ref_count, created_by, created_at, updated_at
		) VALUES (?, ?, '', ?, 0, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		f.Checksum, f.SizePlain, f.PanObjectName,
		f.EncAlgo, f.EncChunkLog2, f.EncNoncePrefix, f.EncSalt, f.DEKEnvelope, f.KEKKeyID,
		int(FileUploading), f.CreatedBy, f.CreatedAt, f.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("占位内容池记录失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// FinalizeFile 在上传成功后补全存储侧信息并置为可用。
func FinalizeFile(ctx context.Context, q Querier, checksum, panFileID, panObjectName string, panSizeWire int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE files SET pan_file_id = ?, pan_object_name = ?, pan_size_wire = ?,
			status = ?, updated_at = ?
		WHERE checksum = ? AND status = ?`,
		panFileID, panObjectName, panSizeWire, int(FileNormal), Now(), checksum, int(FileUploading))
	if err != nil {
		return fmt.Errorf("补全内容池记录失败: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: 内容池记录 %s 不在上传中状态", ErrNoRowsAffected, checksum)
	}
	return nil
}

// InsertFileNormal 直接登记一条"可用"状态的内容池记录，ref_count 为 0。
//
// 供不经上传会话的入库路径（邮件入站 / 草稿）使用：那些路径已经完成了
// 加密与后端写入，不需要"上传中"占位。调用方必须随后 AddFileRef 建立引用，
// 否则对象一旦因其它原因释放引用就会进入待回收。checksum 冲突返 ErrConflict，
// 由调用方按"并发入库同一份内容"处置（复用既有行并删除自己刚传的远端副本）。
func InsertFileNormal(ctx context.Context, q Querier, f File) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO files (
			checksum, size_plain, pan_file_id, pan_object_name, pan_size_wire,
			enc_algo, enc_chunk_log2, enc_nonce_prefix, enc_salt, dek_envelope, kek_key_id,
			status, ref_count, created_by, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		f.Checksum, f.SizePlain, f.PanFileID, f.PanObjectName, f.PanSizeWire,
		f.EncAlgo, f.EncChunkLog2, f.EncNoncePrefix, f.EncSalt, f.DEKEnvelope, f.KEKKeyID,
		int(FileNormal), f.CreatedBy, f.CreatedAt, f.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return fmt.Errorf("%w: 内容池记录 %s 已存在", ErrConflict, f.Checksum)
		}
		return fmt.Errorf("登记内容池记录失败: %w", err)
	}
	return nil
}

// DeleteFileRow 删除占位行。仅用于上传失败清理：只允许删除"上传中"的行，
// 避免误删已被引用的可用对象。
func DeleteFileRow(ctx context.Context, q Querier, checksum string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM files WHERE checksum = ? AND status = ?`,
		checksum, int(FileUploading))
	if err != nil {
		return fmt.Errorf("清理占位记录失败: %w", err)
	}
	return nil
}

// ForceDeleteFileRow 无条件删除内容池记录。
//
// 只用于"待回收"对象的重新上传：此时旧物理对象已无引用，记录留存只会挡住
// 同一份内容的再次写入。调用方必须先删除远端对象。
func ForceDeleteFileRow(ctx context.Context, q Querier, checksum string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM files WHERE checksum = ?`, checksum); err != nil {
		return fmt.Errorf("删除内容池记录失败: %w", err)
	}
	return nil
}

// GetFile 按校验码取内容池记录。
func GetFile(ctx context.Context, q Querier, checksum string) (File, error) {
	row := q.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM files WHERE checksum = ?`, checksum)
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("读取内容池记录失败: %w", err)
	}
	return f, nil
}

// FileStatusInfo 是目录列表真正需要的内容池字段。
type FileStatusInfo struct {
	Status        FileStatus
	DisableReason string
}

// GetFileStatuses 一次取回一批校验码的状态。
//
// 目录列表与分享浏览都要给每个条目附上内容池状态，逐条 GetFile 会让一次
// 列目录产生 N 次查询——这正是列表接口最频繁被调用的路径。这里用一条
// IN 查询取回整批：目录里有多少个文件就只多一次往返。
//
// 只在 map 里出现"查得到的"那些：查不到的校验码由调用方按缺失处理
// （标成禁用而不是隐藏条目），因此本函数不返回 ErrNotFound。
// 空入参直接返回空 map，避免拼出 `IN ()` 这种非法 SQL。
func GetFileStatuses(ctx context.Context, q Querier, checksums []string) (map[string]FileStatusInfo, error) {
	out := make(map[string]FileStatusInfo, len(checksums))
	if len(checksums) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(checksums)), ",")
	args := make([]any, len(checksums))
	for i, cs := range checksums {
		args[i] = cs
	}
	rows, err := q.QueryContext(ctx,
		`SELECT checksum, status, disable_reason FROM files
		 WHERE checksum IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("批量读取内容池状态失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cs, reason string
		var status int
		if err := rows.Scan(&cs, &status, &reason); err != nil {
			return nil, fmt.Errorf("扫描内容池状态失败: %w", err)
		}
		out[cs] = FileStatusInfo{Status: FileStatus(status), DisableReason: reason}
	}
	return out, rows.Err()
}

// GetDedupCandidate 取可用于秒传命中的对象。
//
// 三个条件缺一不可：状态为可用（上传中的半成品不能被命中）、校验码一致、
// 明文长度一致。长度不一致说明哈希碰撞或调用方数据异常，此时应报警而不是放行。
func GetDedupCandidate(ctx context.Context, q Querier, checksum string, sizePlain int64) (File, error) {
	row := q.QueryRowContext(ctx, `SELECT `+fileColumns+`
		FROM files WHERE checksum = ? AND size_plain = ? AND status = ?`,
		checksum, sizePlain, int(FileNormal))
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("读取秒传候选失败: %w", err)
	}
	return f, nil
}

// ChecksumReferencedByGroup 判断该对象是否已被指定组的某个用户引用过。
//
// 秒传的作用域判定：跨组命中意味着"知道明文哈希即可获得可下载的引用"，
// 对私有部署没有必要承担这个泄露面。
func ChecksumReferencedByGroup(ctx context.Context, q Querier, checksum, groupName string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM user_nodes n
		JOIN users u ON u.id = n.user_id
		WHERE n.file_checksum = ? AND u.group_name = ?
		LIMIT 1`, checksum, groupName).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("判定秒传作用域失败: %w", err)
	}
	return true, nil
}

// ReviveArchivedFile 把引用归零后进入待回收状态的内容池记录恢复为可用。
// 用于相同明文重新入池（重复邮件附件、草稿正文重写）：物理对象尚未被
// 清理任务删除，复活比重走加密上传省一次后端往返。ref_count 不在此变更，
// 引用关系由调用方随后 AddFileRef 建立。记录已不在待回收状态时返回
// ErrNoRowsAffected（并发清理或并发复活），调用方应重读后自行判定。
func ReviveArchivedFile(ctx context.Context, q Querier, checksum string) (File, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE files SET status = ?, updated_at = ?
		WHERE checksum = ? AND status = ?`,
		int(FileNormal), Now(), checksum, int(FileArchive))
	if err != nil {
		return File{}, fmt.Errorf("复活待回收内容记录失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return File{}, fmt.Errorf("%w: 内容池记录 %s 不在待回收状态", ErrNoRowsAffected, checksum)
	}
	return GetFile(ctx, q, checksum)
}

// AddFileRef 增加引用计数并返回新值。
func AddFileRef(ctx context.Context, q Querier, checksum string) (int64, error) {
	if _, err := q.ExecContext(ctx,
		`UPDATE files SET ref_count = ref_count + 1, updated_at = ? WHERE checksum = ?`,
		Now(), checksum); err != nil {
		return 0, fmt.Errorf("增加引用计数失败: %w", err)
	}
	var n int64
	if err := q.QueryRowContext(ctx, `SELECT ref_count FROM files WHERE checksum = ?`, checksum).Scan(&n); err != nil {
		return 0, fmt.Errorf("读取引用计数失败: %w", err)
	}
	return n, nil
}

// ReleaseFileRef 减少 1 个引用计数。归零时把状态推进到"待回收"，由清理任务
// 负责真正删除存储侧对象——删除是不可逆动作，不放在请求路径里做。
//
// 委托给 ReleaseFileRefs：两者的 SQL 与"归零转待回收"步骤本就完全相同，
// 只有 delta 不同。引用计数的语义只留一份实现，不会各自漂移。
//
// 返回新的引用计数与状态。
func ReleaseFileRef(ctx context.Context, q Querier, checksum string) (int64, FileStatus, error) {
	return ReleaseFileRefs(ctx, q, checksum, 1)
}

// ReleaseFileRefs 一次性减少多个引用计数，语义与 ReleaseFileRef 相同
// （归零且状态正常时推进到待回收），但只发两条 SQL。
//
// 删除整棵子树时同一内容可能被引用多次：循环调用单次版本会让事务里的
// 语句数随子树大小线性增长。delta 必须为正且不得超过当前计数，否则说明
// 调用方统计的引用清单与库内状态不一致，整笔事务回滚而不是把计数减成负数。
// 返回新的引用计数与状态。
func ReleaseFileRefs(ctx context.Context, q Querier, checksum string, delta int64) (int64, FileStatus, error) {
	if delta <= 0 {
		return 0, 0, fmt.Errorf("%w: 释放引用数必须为正", ErrNoRowsAffected)
	}
	res, err := q.ExecContext(ctx, `
		UPDATE files SET ref_count = ref_count - ?, updated_at = ?
		WHERE checksum = ? AND ref_count >= ?`, delta, Now(), checksum, delta)
	if err != nil {
		return 0, 0, fmt.Errorf("减少引用计数失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, 0, fmt.Errorf("%w: 对象 %s 引用计数不足 %d", ErrNoRowsAffected, checksum, delta)
	}
	// 引用归零即可标记待回收；被拉黑的对象保持拉黑状态，等待人工裁决。
	if _, err := q.ExecContext(ctx, `
		UPDATE files SET status = ?, updated_at = ?
		WHERE checksum = ? AND ref_count = 0 AND status = ?`,
		int(FileArchive), Now(), checksum, int(FileNormal)); err != nil {
		return 0, 0, fmt.Errorf("标记待回收失败: %w", err)
	}
	var refCount int64
	var status int
	if err := q.QueryRowContext(ctx, `SELECT ref_count, status FROM files WHERE checksum = ?`, checksum).
		Scan(&refCount, &status); err != nil {
		return 0, 0, fmt.Errorf("读取引用计数失败: %w", err)
	}
	return refCount, FileStatus(status), nil
}

// SetFileStatus 变更对象状态（拉黑 / 解除拉黑）。
//
// 拉黑是全局动作：同一对象被多人引用时只有一份内容与一份状态，处置它会
// 影响所有引用者。要"只影响某人"必须落在节点级状态上。
func SetFileStatus(ctx context.Context, q Querier, checksum string, status FileStatus, reason string, by int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE files SET status = ?, disable_reason = ?, disabled_by = ?, disabled_at = ?, updated_at = ?
		WHERE checksum = ?`,
		int(status), reason, by, Now(), Now(), checksum)
	if err != nil {
		return fmt.Errorf("变更对象状态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeDueFile 是一条到达计划清理时间、等待真实删除远端对象的内容池记录。
type PurgeDueFile struct {
	Checksum      string
	PanFileID     string
	PanObjectName string
}

// ListPurgeDueFiles 列出已到计划清理时间、引用归零的记录。
//
// archive_purge_at 由归档删除转换时写入（删除时间 + 管理留存期），
// 没到点的内容哪怕引用归零也必须留在存储上，用户还可能恢复。
func ListPurgeDueFiles(ctx context.Context, q Querier, now int64, limit int) ([]PurgeDueFile, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `
		SELECT checksum, pan_file_id, pan_object_name FROM files
		WHERE status = ? AND ref_count = 0 AND archive_purge_at > 0 AND archive_purge_at <= ?
		ORDER BY archive_purge_at LIMIT ?`,
		int(FileArchive), now, limit)
	if err != nil {
		return nil, fmt.Errorf("查询到期待清理对象失败: %w", err)
	}
	defer rows.Close()
	var out []PurgeDueFile
	for rows.Next() {
		var f PurgeDueFile
		if err := rows.Scan(&f.Checksum, &f.PanFileID, &f.PanObjectName); err != nil {
			return nil, fmt.Errorf("扫描到期待清理对象失败: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetFileArchivePurgeAt 记录内容池对象计划物理删除的时间点。
// 同一份内容可能同时挂在多个归档批次里，取 MAX 让时间被只延后不提前：
// 任何一个批次还没到期，存储上的对象都必须留到那个时间之后。
func SetFileArchivePurgeAt(ctx context.Context, q Querier, checksum string, at int64) error {
	_, err := q.ExecContext(ctx, `
		UPDATE files SET archive_purge_at = MAX(archive_purge_at, ?), updated_at = ?
		WHERE checksum = ?`, at, Now(), checksum)
	if err != nil {
		return fmt.Errorf("记录回收清理时间失败: %w", err)
	}
	return nil
}

// GetFileArchivePurgeAt 读取对象排定的物理删除时间（未排定为 0）。
func GetFileArchivePurgeAt(ctx context.Context, q Querier, checksum string) (int64, error) {
	var at int64
	err := q.QueryRowContext(ctx,
		`SELECT archive_purge_at FROM files WHERE checksum = ?`, checksum).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("读取回收清理时间失败: %w", err)
	}
	return at, nil
}

// ListArchiveFilesWithoutPurgeAt 列出"引用归零但从未排定清理时间"的待回收
// 记录。这类行来自归档批次之外的引用释放（例如覆盖同名文件），批次流转
// 不会为它们写入 archive_purge_at，若不补记就会连同远端对象一起永久滞留。
func ListArchiveFilesWithoutPurgeAt(ctx context.Context, q Querier, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `
		SELECT checksum FROM files
		WHERE status = ? AND ref_count = 0 AND archive_purge_at = 0 AND pan_file_id != ''
		ORDER BY updated_at LIMIT ?`, int(FileArchive), limit)
	if err != nil {
		return nil, fmt.Errorf("查询未排定清理的待回收对象失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cs string
		if err := rows.Scan(&cs); err != nil {
			return nil, fmt.Errorf("扫描待补记对象失败: %w", err)
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// MarkRegistrationFailed 把"远端对象已写入但收尾事务失败"的记录转入待回收：
// 保住定位符供维护任务重试删除，并按给定时间排定清理。仅对仍处于上传中的
// 行生效，避免覆盖并发请求已经推进的状态。
func MarkRegistrationFailed(ctx context.Context, q Querier, checksum, panFileID, panObjectName string, panSizeWire, purgeAt int64) error {
	res, err := q.ExecContext(ctx, `
		UPDATE files SET pan_file_id = ?, pan_object_name = ?, pan_size_wire = ?,
			status = ?, archive_purge_at = ?, updated_at = ?
		WHERE checksum = ? AND status = ?`,
		panFileID, panObjectName, panSizeWire, int(FileArchive), purgeAt, Now(), checksum, int(FileUploading))
	if err != nil {
		return fmt.Errorf("写入登记失败态失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearArchiveObjectRef 在远端对象删除成功后清空回收态记录的定位符，
// 表示物理对象已不存在。仅当记录仍处于回收态且定位符匹配时生效，
// 防止并发处置互相覆盖。
func ClearArchiveObjectRef(ctx context.Context, q Querier, checksum, panFileID string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE files SET pan_file_id = '', updated_at = ?
		WHERE checksum = ? AND status = ? AND pan_file_id = ?`,
		Now(), checksum, int(FileArchive), panFileID)
	if err != nil {
		return false, fmt.Errorf("清空回收定位符失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkFilePurged 远端对象真实删除成功后，把记录置为"存储删除"。
//
// 行本身保留：对象名与校验码是追溯与审计的唯一线索。
func MarkFilePurged(ctx context.Context, q Querier, checksum string) error {
	res, err := q.ExecContext(ctx, `
		UPDATE files SET status = ?, updated_at = ?
		WHERE checksum = ? AND status = ? AND ref_count = 0`,
		int(FilePurged), Now(), checksum, int(FileArchive))
	if err != nil {
		return fmt.Errorf("标记存储删除失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountFilesByStatus 统计各状态的对象数量。
func CountFilesByStatus(ctx context.Context, q Querier) (map[FileStatus]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT status, COUNT(*) FROM files GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("统计对象状态失败: %w", err)
	}
	defer rows.Close()
	out := map[FileStatus]int64{}
	for rows.Next() {
		var status int
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("扫描统计结果失败: %w", err)
		}
		out[FileStatus(status)] = n
	}
	return out, rows.Err()
}

// SumWireBytesInUse 返回当前被引用的密文总字节数（用于存储配额的对账口径）。
func SumWireBytesInUse(ctx context.Context, q Querier) (int64, error) {
	var total sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT SUM(pan_size_wire) FROM files WHERE ref_count > 0 AND status IN (?, ?)`,
		int(FileNormal), int(FileDisabled)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("统计存储占用失败: %w", err)
	}
	return total.Int64, nil
}
