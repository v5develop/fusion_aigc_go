// Package taskcenter 把本地生活的 AIGC 任务同步进现有系统的任务中心（afv_creation_task）。
//
// 任务中心列表（/api/v1/creation/task-center/tasks）只读 afv_creation_task 里的父任务，
// 本地生活的业务记录在各自的表里（afv_image_creation / afv_video_creation /
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
	"fmt"
	"strings"
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

// Source 一张源表（的一类记录）。除 Table / Prefix / EntryCode / Type 外都是 SQL 表达式，写源表的列名。
type Source struct {
	Table     string // 源表，如 afv_image_creation
	Prefix    string // task_id 前缀，如 img；最终 AiCreation_life_<Prefix>_<id>
	Kind      string // 源表 kind 列的取值；空=不按 kind 筛（笔记 / 数字人）
	EntryCode string // 入口码，同计费 offer 的 entry_code
	Type      string // image / video（决定超时口径和任务中心的产物类型）

	Include string // 哪些行进任务中心，如 "status IN (2,3,4)"；空=全部
	Status  string // 映射成任务中心状态的表达式
	Result  string // 成品地址（单个）
	Results string // 成品地址数组（JSON），视频类留空
	Cover   string // 封面；空=用 Result
	Fail    string // 失败原因
	Prompt  string // 提示词 / 文案，放进 request_params.prompt 供列表展示
}

func (s Source) taskID() string { return fmt.Sprintf("AiCreation_life_%s_", s.Prefix) } // 与 Ref 同一格式

// Ref 一条源表记录在任务中心里的 task_id（AiCreation_life_<Prefix>_<id>）。
// 提交执行面的 task_id 也要带上它（aigc.NewLifeTaskID），好用任务中心的编号查执行面日志。
func Ref(prefix string, id int64) string { return fmt.Sprintf("AiCreation_life_%s_%d", prefix, id) }

func orNull(e string) string {
	if strings.TrimSpace(e) == "" {
		return "NULL"
	}
	return e
}

// sql 生成 upsert 语句。窗口参数：只同步 update_time 在最近 N 秒内的行（N=0 同步全部）。
func (s Source) sql() string {
	where := []string{"(? = 0 OR update_time >= NOW() - INTERVAL ? SECOND)"}
	if s.Kind != "" {
		where = append(where, "kind = '"+s.Kind+"'")
	}
	if s.Include != "" {
		where = append(where, "("+s.Include+")")
	}
	cover := s.Cover
	if strings.TrimSpace(cover) == "" {
		cover = s.Result
	}
	return "INSERT INTO afv_creation_task " +
		"(task_id, user_id, type, status, entry_code, request_params, result_url, result_urls, cover_url, error_msg, " +
		"is_transferred, deleted, create_time, update_time) " +
		"SELECT CONCAT('" + s.taskID() + "', id), user_id, '" + s.Type + "', " + s.Status + ", '" + s.EntryCode + "', " +
		"JSON_OBJECT('entryCode', '" + s.EntryCode + "', 'prompt', " + orNull(s.Prompt) + "), " +
		orNull(s.Result) + ", " + orNull(s.Results) + ", " + orNull(cover) + ", " + orNull(s.Fail) + ", " +
		"1, deleted, create_time, update_time FROM " + s.Table + " WHERE " + strings.Join(where, " AND ") + " " +
		"ON DUPLICATE KEY UPDATE status = VALUES(status), result_url = VALUES(result_url), result_urls = VALUES(result_urls), " +
		"cover_url = VALUES(cover_url), error_msg = VALUES(error_msg), request_params = VALUES(request_params), " +
		"deleted = GREATEST(afv_creation_task.deleted, VALUES(deleted)), update_time = VALUES(update_time)"
}

// Syncer 一个服务里的若干源表。
type Syncer struct {
	conn    sqlx.SqlConn
	sources []Source
	first   bool
	window  time.Duration
}

// New window：每轮回看多久内有变动的行；第一轮（进程刚起）全量同步，补上停机期间漏掉的。
func New(conn sqlx.SqlConn, window time.Duration, sources ...Source) *Syncer {
	if window <= 0 {
		window = 10 * time.Minute
	}
	return &Syncer{conn: conn, sources: sources, first: true, window: window}
}

// Sync 同步一轮。失败只记日志，不影响业务轮询。
func (y *Syncer) Sync(ctx context.Context) {
	if y == nil {
		return
	}
	secs := int64(y.window / time.Second)
	if y.first {
		secs = 0
	}
	ok := true
	for _, s := range y.sources {
		if _, err := y.conn.ExecCtx(ctx, s.sql(), secs, secs); err != nil {
			ok = false
			logx.WithContext(ctx).Errorf("taskcenter sync %s/%s: %v", s.Table, s.EntryCode, err)
		}
	}
	if ok {
		y.first = false
	}
}
