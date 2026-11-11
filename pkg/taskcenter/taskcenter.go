// Package taskcenter 把本地生活的 AIGC 任务同步进现有系统的任务中心（afv_creation_task）。
//
// 任务中心列表（/api/v1/creation/task-center/tasks）只读 afv_creation_task 里的父任务，
// 本地生活的业务记录在各自的表里（afv_local_life_creation / afv_video_creation /
// afv_image_text_note / afv_digital_human），所以由各服务的轮询器按“源表 → 任务中心”整行同步：
//   - 以源表为准，每轮把最近有变动的行 upsert 过去（建、改状态、写成品、删除都走这一条路）；
//   - task_id 用源表名 + 主键拼出，稳定且全局唯一，不依赖执行面任务号（构思阶段还没有）；
//   - 不写 execution_task_id / orchestration_plan，现有系统的对账、驱动、恢复任务都按它们筛，
//     留空就不会被当成它们自己的任务去重新查执行面；
//   - 现有系统的超时回收（image 30 分钟 / video 150 分钟）长于我们自己的超时（20 / 40 分钟），
//     所以 type 必须写 image / video，别写别的。
package taskcenter

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 任务中心状态：0排队 1处理中 2完成 3失败 4取消 5部分成功
const (
	Processing = 1
	Done       = 2
	Failed     = 3
)

// OriginLocalLife 同步进 afv_creation_task 的行的来源标记（Java 侧 CreationTaskOwnership 按它判定只读）。
const OriginLocalLife = "LOCAL_LIFE"

// TextTable 文本产物正文的扩展表（task_id 1:1 指向 afv_creation_task.task_id）。
const TextTable = "afv_creation_task_text"

// Source 一张源表（的一类记录）。除 Table / Prefix / EntryCode / Type 外都是 SQL 表达式，写源表的列名。
type Source struct {
	Table     string // 源表，如 afv_local_life_creation
	Prefix    string // task_id 前缀，如 img；最终 AiCreation_LF_<md5(Prefix:id)>
	Kind      string // 源表 kind 列的取值；空=不按 kind 筛（笔记 / 数字人）
	EntryCode string // 入口码，同计费 offer 的 entry_code
	Type      string // image / video（决定超时口径和任务中心的产物类型）

	Include string // 哪些行进任务中心，如 "status IN (2,3,4)"；空=全部
	Status  string // 映射成任务中心状态的表达式
	Result  string // 成品地址（单个）
	// RequestPairs 入参快照的 JSON 键值对（不含 entryCode，由 EntryCode 补上）。空=只写 prompt（用 Prompt）。
	// 与 Java 的 requestParams 一致：用户端提交的完整入参，便于任务中心查看「当时提交了什么」。
	RequestPairs string
	// ExecTaskID 执行方任务号（execution_task_id）；多个执行面任务时取第一个，其余在源表的明细里。
	// 执行参数（execution_params）不在这里配：统一取提交时落库的快照，见 execParamsSQL。
	ExecTaskID string
	// BillingRequest 计费请求号的 SQL 表达式（以 s 为源表别名，如 s.`request_id`、CONCAT('copywriting-', s.id)），
	// 必须与服务里 billing.Charge.RequestID 的生成规则一致。空=不同步计费字段。
	// 同步时按 biz_type=LOCALLIFE、biz_id=用户号_请求号（超长时同 billing.bizID 取 SHA-256）关联预留单。
	BillingRequest string
	Results        string // 成品地址数组（JSON），视频类留空
	ResultText     string // 文本产物正文（如文案 body），媒体类留空；写入扩展表 afv_creation_task_text，不进任务主表
	Cover          string // 封面；空=用 Result
	Fail           string // 失败原因
	Prompt         string // 提示词 / 文案，放进 request_params.prompt 供列表展示

	// CreateTimeCol / UpdateTimeCol 源表的创建、更新时间列名；空=create_time / update_time（老表）。
	// 新模块（bun 模型，如 afv_studio_task）用 created_at / updated_at，在这里改名即可，同步语义不变。
	CreateTimeCol string
	UpdateTimeCol string
}

// createCol / updateCol 源表时间列（带 s. 别名）。
func (s Source) createCol() string {
	if c := strings.TrimSpace(s.CreateTimeCol); c != "" {
		return "s." + c
	}
	return "s.create_time"
}

func (s Source) updateCol() string {
	if c := strings.TrimSpace(s.UpdateTimeCol); c != "" {
		return "s." + c
	}
	return "s.update_time"
}

// Ref 一条源表记录在任务中心里的 task_id：AiCreation_LF_ + md5("<Prefix>:<id>") 的 32 位小写十六进制。
// LF 标明来源是本地生活（所有权由 origin_service 判定，编号只用于展示和排查）；由 (prefix, id) 确定，不落库；
// 同步语句里用 MySQL 的 MD5 算出同一个值（见 Source.refSQL），两边必须保持一致。
// 提交执行面的 task_id 也要带上它（aigc.NewLifeTaskID），好用任务中心的编号查执行面日志。
func Ref(prefix string, id int64) string {
	sum := md5.Sum([]byte(prefix + ":" + strconv.FormatInt(id, 10)))
	return "AiCreation_LF_" + hex.EncodeToString(sum[:])
}

// refSQL 与 Ref 等价的 MySQL 表达式：id 是源表主键（整数），CONCAT 出的十进制文本与 strconv 一致。
func (s Source) refSQL() string {
	return "CONCAT('AiCreation_LF_', MD5(CONCAT('" + s.Prefix + ":', id)))"
}

// OriginRef 源记录的坐标，如 img:123。写进 afv_creation_task.origin_ref：Java 后台转发操作时用它定位 Go 的记录，
// 不靠反推 task_id（md5 不可逆）。格式：<Prefix>:<主键>，与 Ref 的前缀一致。
func OriginRef(prefix string, id int64) string {
	return prefix + ":" + strconv.FormatInt(id, 10)
}

// originRefSQL 与 OriginRef 等价的 MySQL 表达式。
func (s Source) originRefSQL() string {
	return "CONCAT('" + s.Prefix + ":', s.id)"
}

func orNull(e string) string {
	if strings.TrimSpace(e) == "" {
		return "NULL"
	}
	return e
}

// billingKeySQL 预留单的 biz_id：与 pkg/billing 的 bizID 同一规则（超过 64 字节取 SHA-256 十六进制截断）。
func (s Source) billingKeySQL() string {
	id := "CONCAT(s.user_id, '_', " + s.BillingRequest + ")"
	return "CASE WHEN LENGTH(" + id + ") <= 64 THEN " + id +
		" ELSE LEFT(CONCAT(s.user_id, '_', SHA2(" + id + ", 256)), 64) END"
}

// billingSub 取预留单的某一列；没有预留单返回 NULL。
func (s Source) billingSub(expr string) string {
	return "(SELECT " + expr + " FROM afv_billing_reservation r WHERE r.biz_type = 'LOCALLIFE' AND r.deleted = 0" +
		" AND r.biz_id = " + s.billingKeySQL() + " LIMIT 1)"
}

// billingColumns 计费字段的 SELECT 表达式（顺序与 INSERT 列一致）。
// 预留状态映射到 Java 的 billing_status：EXPIRED 是预扣过期释放，点数已回到余额，等同退款。
func (s Source) billingColumns() []string {
	if strings.TrimSpace(s.BillingRequest) == "" {
		return []string{"'NONE'", "NULL", "NULL", "NULL", "NULL"}
	}
	status := "CASE r.status WHEN 'HELD' THEN 'HELD' WHEN 'CONFIRMED' THEN 'CONFIRMED' " +
		"WHEN 'PARTIALLY_REFUNDED' THEN 'PARTIALLY_REFUNDED' WHEN 'REFUNDED' THEN 'REFUNDED' " +
		"WHEN 'CANCELLED' THEN 'REFUNDED' WHEN 'EXPIRED' THEN 'REFUNDED' ELSE 'BILLING_PENDING_REVIEW' END"
	return []string{
		"IFNULL(" + s.billingSub(status) + ", 'NONE')",
		s.billingSub("r.price_points"),
		s.billingSub("r.feature_code"),
		s.billingSub("r.model_id"),
		s.billingSub("r.reservation_no"),
	}
}

// args 与 sql() 的占位符一一对应：窗口参数，有计费关联时多一个。
func (s Source) args(secs int64) []any {
	if strings.TrimSpace(s.BillingRequest) == "" {
		return []any{secs, secs}
	}
	return []any{secs, secs, secs}
}

// requestSQL 入参快照：配了 RequestPairs 就用完整入参，否则只有 prompt（与旧行为一致）。
func (s Source) requestSQL() string {
	if strings.TrimSpace(s.RequestPairs) != "" {
		return "JSON_OBJECT('entryCode', '" + s.EntryCode + "', " + s.RequestPairs + ")"
	}
	return "JSON_OBJECT('entryCode', '" + s.EntryCode + "', 'prompt', " + orNull(s.Prompt) + ")"
}

// where 同步的筛选条件（窗口 + 类型 + 包含条件），sql 与 textSQL 共用，占位符参数见 args。
func (s Source) where() []string {
	where := []string{"(? = 0 OR " + s.updateCol() + " >= NOW() - INTERVAL ? SECOND)"}
	if strings.TrimSpace(s.BillingRequest) != "" {
		// 预留状态会独立变化（冻结→结算→过期），源记录没动也要同步：预留单在窗口内更新过也纳入
		where[0] = "(? = 0 OR " + s.updateCol() + " >= NOW() - INTERVAL ? SECOND OR EXISTS (SELECT 1 FROM afv_billing_reservation r " +
			"WHERE r.biz_type = 'LOCALLIFE' AND r.deleted = 0 AND r.biz_id = " + s.billingKeySQL() +
			" AND r.update_time >= NOW() - INTERVAL ? SECOND))"
	}
	if s.Kind != "" {
		where = append(where, "kind = '"+s.Kind+"'")
	}
	if s.Include != "" {
		where = append(where, "("+s.Include+")")
	}
	return where
}

// textSQL 文本产物正文的 upsert，写扩展表 afv_creation_task_text（按任务编号 1:1）。
// 任务主表不放正文：列表查询是 SELECT *，大字段会拖慢所有任务的列表。只有有正文（非空）的行才写。
func (s Source) textSQL() string {
	return "INSERT INTO " + TextTable + " (task_id, content, create_time, update_time) " +
		"SELECT " + s.refSQL() + ", " + s.ResultText + ", " + s.createCol() + ", " + s.updateCol() + " FROM " + s.Table + " s " +
		"WHERE " + strings.Join(s.where(), " AND ") + " AND COALESCE(" + s.ResultText + ", '') <> '' " +
		"ON DUPLICATE KEY UPDATE content = VALUES(content), update_time = VALUES(update_time)"
}

// sql 生成主表 upsert 语句。窗口参数：只同步 update_time 在最近 N 秒内的行（N=0 同步全部）。
func (s Source) sql() string {
	where := s.where()
	cover := s.Cover
	if strings.TrimSpace(cover) == "" {
		cover = s.Result
	}
	return "INSERT INTO afv_creation_task " +
		"(task_id, user_id, type, status, entry_code, request_params, execution_task_id, execution_params, " +
		"billing_status, price_points, feature_code, model_id, reservation_no, " +
		"result_url, result_urls, cover_url, error_msg, is_transferred, deleted, create_time, update_time, origin_service, origin_ref) " +
		"SELECT " + s.refSQL() + ", s.user_id, '" + s.Type + "', " + s.Status + ", '" + s.EntryCode + "', " +
		s.requestSQL() + ", " + orNull(s.ExecTaskID) + ", " + s.execParamsSQL() + ", " +
		strings.Join(s.billingColumns(), ", ") + ", " +
		orNull(s.Result) + ", " + orNull(s.Results) + ", " + orNull(cover) + ", " + orNull(s.Fail) + ", " +
		"1, s.deleted, " + s.createCol() + ", " + s.updateCol() + ", '" + OriginLocalLife + "', " + s.originRefSQL() + " FROM " + s.Table + " s WHERE " + strings.Join(where, " AND ") + " " +
		"ON DUPLICATE KEY UPDATE status = VALUES(status), result_url = VALUES(result_url), result_urls = VALUES(result_urls), " +
		"cover_url = VALUES(cover_url), error_msg = VALUES(error_msg), request_params = VALUES(request_params), " +
		// 没有提交快照的行（上线前提交的）不覆盖已有的执行参数，免得同步一次就被清空
		"execution_task_id = VALUES(execution_task_id), execution_params = COALESCE(VALUES(execution_params), execution_params), " +
		"billing_status = VALUES(billing_status), price_points = VALUES(price_points), feature_code = VALUES(feature_code), " +
		"model_id = VALUES(model_id), reservation_no = VALUES(reservation_no), " +
		"deleted = GREATEST(afv_creation_task.deleted, VALUES(deleted)), update_time = VALUES(update_time), " +
		"origin_service = VALUES(origin_service), origin_ref = VALUES(origin_ref)"
}

// Syncer 一个服务里的若干源表。
type Syncer struct {
	conn    sqlx.SqlConn
	sources []Source
	// synced 每个源是否已经成功做过一次全量同步。只有成功过的源才改用窗口同步：
	// 某个源一直失败时，只让它自己反复全量，不拖累其他源。
	synced []bool
	// mu 串行化整轮 Sync（也保护 synced）：轮询器与请求路径（异步）可能同时 Sync。两轮并发对同一批任务中心行
	// INSERT … SELECT … ON DUPLICATE KEY UPDATE 会互相等锁甚至死锁（1213）白白报错，串行后后到的一轮读到的一定不旧于先到的
	mu     sync.Mutex
	window time.Duration
}

func New(conn sqlx.SqlConn, window time.Duration, sources ...Source) *Syncer {
	if window <= 0 {
		window = 10 * time.Minute
	}
	return &Syncer{conn: conn, sources: sources, synced: make([]bool, len(sources)), window: window}
}

// Sync 同步一轮，返回本轮所有失败的合并错误（nil=全部成功）。
// 调用方可以只记日志、不影响业务轮询；返回值让测试和排障能直接看到失败原因。
func (y *Syncer) Sync(ctx context.Context) error {
	if y == nil {
		return nil
	}
	y.mu.Lock()
	defer y.mu.Unlock()
	window := int64(y.window / time.Second)
	var errs []error
	for i, s := range y.sources {
		secs := window
		if !y.synced[i] {
			secs = 0 // 首次（或上次失败后）全量同步，保证不漏
		}
		err := y.exec(ctx, s.sql(), s.args(secs))
		if err == nil && s.ResultText != "" {
			err = y.exec(ctx, s.textSQL(), s.args(secs))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", s.Table, s.EntryCode, err))
			continue
		}
		y.synced[i] = true
	}
	return errors.Join(errs...)
}

// exec 执行一条同步语句，失败时记日志并返回带源信息的错误。
func (y *Syncer) exec(ctx context.Context, query string, args []any) error {
	if _, err := y.conn.ExecCtx(ctx, query, args...); err != nil {
		logx.WithContext(ctx).Errorf("taskcenter sync: %v", err)
		return err
	}
	return nil
}
