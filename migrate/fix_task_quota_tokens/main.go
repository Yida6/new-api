// fix_task_quota_tokens 一次性修复异步任务"总 Token 数"历史累计数据。
//
// 背景：修复前，异步任务（Seedance/Suno/视频/MJ）提交瞬间上游无 usage，提交消费
// 日志写 quota_data 时 TokenUsed=0；结算完成后仅把实际 total_tokens 回填到
// logs.other.total_tokens（BackfillTaskConsumeLogTotalTokens），从未写入
// quota_data.token_used，导致数据看板/排行榜"总 Token 数"始终为 0。
//
// 新版运行时已自动处理：
//   - 结算时把 total_tokens 持久化到 tasks.total_tokens（RecordTaskTotalTokensToQuotaData）；
//   - 后台 ReconcileTaskTokensToQuotaData 按"total_tokens > 已同步值"把差额累计进
//     quota_data.token_used（幂等），并自愈：对 tasks.total_tokens=0 的终态成功任务，
//     从 task.Data 原始上游响应解析 usage.total_tokens 自动恢复（recoverTasksFromData）。
//
// 本工具用于兜底 task.Data 已被清理/覆盖、仅日志 other.total_tokens 仍保留的存量：
//  1. 扫描日志库中"已回填 total_tokens"的异步任务提交日志；
//  2. 从日志 other 解析出 total_tokens，并定位对应 task 行；
//  3. 把 task.total_tokens 置为该值（若当前为 0/偏小）；
//  4. 调用 model.ReconcileTaskTokensToQuotaData 把差额补进 quota_data.token_used。
//
// 幂等：与运行时同一套机制——task.token_quota_synced 记录已同步值，仅当
// total_tokens > token_quota_synced 才追加差额；配合台账表 task_token_fix_applied
// 防止同一日志重复处理，可重复执行、不重复累计。
//
// 用法：
//
//	go run ./migrate/fix_task_quota_tokens --check
//	go run ./migrate/fix_task_quota_tokens --apply [--before <升级时间戳>] [--task-id <id>] [--user-id <id>]
//
// 依赖环境变量（与主程序一致）：SQL_DSN / LOG_SQL_DSN / SQLITE_PATH 等。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"gorm.io/gorm"
)

var (
	apply  = flag.Bool("apply", false, "实际执行修复（默认 dry-run 只报告差异）")
	check  = flag.Bool("check", true, "只读检查（默认开启）；配合 --apply 才会写库")
	taskID = flag.String("task-id", "", "仅修复指定 task_id（空 = 全部）")
	userID = flag.Int("user-id", 0, "仅修复指定用户（0 = 全部）")
	before = flag.Int64("before", 0, "仅处理 created_at 早于该 Unix 时间戳的提交日志（设为升级到新版运行时的时刻，避免重复处理运行时已导出的记录）")
)

// 台账表：记录已处理的日志 id（幂等）。
const ledgerTable = "task_token_fix_applied"

// 日志类型：消费（type=2）。异步任务提交时写 type=2、other 含 is_task 与 total_tokens。
const logTypeConsume = 2

type taskLog struct {
	ID     int64
	UserID int
	TaskID string
	Total  int
}

func main() {
	flag.Parse()

	if *apply && *taskID == "" && *before == 0 {
		// 未指定截止时间就写库：运行时已自动导出新任务的 total_tokens，若再全量
		// 扫描日志会把"升级后已处理"的任务重复处理。要求显式 --before 或 --task-id
		// 收窄范围（task_id 精确匹配天然幂等）。
		fmt.Fprintln(os.Stderr, "错误：--apply 未指定 --before（升级到新版运行时的 Unix 时间戳）或 --task-id。")
		fmt.Fprintln(os.Stderr, "请加 --before <升级时间戳> 只处理升级前的记录；或 --task-id <id> 精确修复单条。")
		os.Exit(1)
	}

	common.InitEnv()
	// 只连接数据库，不执行 AutoMigrate：修复工具不应改动表结构。
	common.IsMasterNode = false
	if err := model.InitDB(); err != nil {
		fmt.Printf("初始化主数据库失败: %v\n", err)
		os.Exit(1)
	}
	if err := model.InitLogDB(); err != nil {
		fmt.Printf("初始化日志数据库失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("===== fix_task_quota_tokens: 修复异步任务总 Token 统计 =====")
	fmt.Printf("模式: %s | 主库: %s | 日志库: %s\n",
		map[bool]string{true: "APPLY（写库）", false: "DRY-RUN（只报告）"}[*apply],
		common.MainDatabaseType(), common.LogDatabaseType())
	if *taskID != "" {
		fmt.Printf("范围: 仅任务 %s\n", *taskID)
	}
	if *userID > 0 {
		fmt.Printf("范围: 仅用户 %d\n", *userID)
	}
	if *before > 0 {
		fmt.Printf("范围: 仅处理 created_at < %d 的提交日志\n", *before)
	}
	if common.DataExportEnabled {
		fmt.Println("提示: 数据导出开关已开启（运行时将自动处理新任务与 task.Data 自愈）")
	}

	printBackupInstructions()

	if *apply {
		if err := ensureLedgerTable(); err != nil {
			fmt.Printf("初始化迁移台账表失败: %v\n", err)
			os.Exit(1)
		}
	}

	logs, err := loadTaskLogs()
	if err != nil {
		fmt.Printf("查询任务提交日志失败: %v\n", err)
		os.Exit(1)
	}
	applied := loadLedger()
	processLogs(logs, applied)

	fmt.Println("\n===== 完成 =====")
	if !*apply {
		fmt.Println("（DRY-RUN 模式，未写入任何数据）")
	}
}

func printBackupInstructions() {
	_, _ = os.Stdout.WriteString(`
【执行前必须备份】
  MySQL:    mysqldump -h<HOST> -u<USER> -p<PASS> <DB> tasks quota_data > backup_task_tokens_$(date +%Y%m%d%H%M%S).sql
  SQLite:   cp <SQLITE_PATH>/one-api.db one-api.db.bak.$(date +%Y%m%d%H%M%S)
  PostgreSQL: pg_dump -h <HOST> -U <USER> -d <DB> -t tasks -t quota_data -F c -f backup_task_tokens_$(date +%Y%m%d%H%M%S).dump
建议：先停机或暂停任务轮询再执行 --apply，避免迁移期间新的结算写入造成并发差异。
若新版运行时已上线：请用 --before <升级时间戳> 限定只处理升级前的提交日志。`)
}

// ensureLedgerTable 创建迁移台账表。
func ensureLedgerTable() error {
	return model.DB.Exec(fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (log_id BIGINT NOT NULL PRIMARY KEY, applied_at BIGINT NOT NULL)", ledgerTable)).Error
}

// loadLedger 加载已处理过的日志 id 集合。
func loadLedger() map[int64]struct{} {
	out := make(map[int64]struct{})
	var ids []int64
	if err := model.DB.Table(ledgerTable).Pluck("log_id", &ids).Error; err != nil {
		if *apply {
			fmt.Printf("读取迁移台账失败: %v\n", err)
		}
		return out
	}
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

// loadTaskLogs 扫描日志库中"已回填 total_tokens"的异步任务提交日志。
// 用 GORM 查询构建器：占位符由驱动翻译，兼容 SQLite/MySQL/PostgreSQL，
// other 的 LIKE 文本匹配不依赖 JSON 函数。
func loadTaskLogs() ([]taskLog, error) {
	tx := model.LOG_DB.Table("logs").Select("id, user_id, other").
		Where("type = ?", logTypeConsume).
		Where("other LIKE ?", "%\"is_task\":true%").
		Where("other LIKE ?", "%total_tokens%")
	if *taskID != "" {
		tx = tx.Where("other LIKE ?", "%"+*taskID+"%")
	}
	if *before > 0 {
		tx = tx.Where("created_at < ?", *before)
	}
	rows, err := tx.Order("id").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []taskLog
	for rows.Next() {
		var id int64
		var userID int
		var other string
		if err := rows.Scan(&id, &userID, &other); err != nil {
			return nil, err
		}
		total, taskIDStr := parseOtherTokens(other)
		if total <= 0 || taskIDStr == "" {
			continue
		}
		out = append(out, taskLog{ID: id, UserID: userID, TaskID: taskIDStr, Total: total})
	}
	return out, rows.Err()
}

// parseOtherTokens 从日志 other JSON 解析 (total_tokens, task_id)。
func parseOtherTokens(other string) (int, string) {
	var m map[string]interface{}
	if err := common.Unmarshal([]byte(other), &m); err != nil {
		return 0, ""
	}
	total, _ := m["total_tokens"].(float64)
	taskIDStr, _ := m["task_id"].(string)
	return int(total), taskIDStr
}

// processLogs 逐条处理：把日志中的 total_tokens 补到 task 行并补录 quota_data。
func processLogs(logs []taskLog, applied map[int64]struct{}) {
	fmt.Printf("\n--- 处理异步任务提交日志（共 %d 条候选）---\n", len(logs))

	processed, skipped := 0, 0
	taskMissing := 0

	for _, lg := range logs {
		if *userID > 0 && lg.UserID != *userID {
			continue
		}
		if _, done := applied[lg.ID]; done {
			skipped++
			continue
		}

		if !*apply {
			fmt.Printf("  [待处理] 日志 %d 任务 %s total_tokens=%d（用户 %d）\n", lg.ID, lg.TaskID, lg.Total, lg.UserID)
			processed++
			continue
		}

		// 1. 定位任务行（task_id + user_id 唯一）
		var task model.Task
		err := model.DB.Where("task_id = ? AND user_id = ?", lg.TaskID, lg.UserID).First(&task).Error
		if err != nil {
			if err == gorm.ErrRecordNotFound {
				fmt.Printf("  [跳过] 日志 %d 任务 %s 不存在（可能已被清理）\n", lg.ID, lg.TaskID)
				taskMissing++
			} else {
				fmt.Printf("  [错误] 日志 %d 查询任务失败: %v\n", lg.ID, err)
			}
			continue
		}

		// 2. 持久化 total_tokens（幂等：只增不减，避免覆盖已存在的更高值）
		if task.TotalTokens < lg.Total {
			model.RecordTaskTotalTokensToQuotaData(&task, lg.Total)
		}

		// 3. 记录台账（在任务持久化后立即落账，防重复处理）
		if err := insertLedger(model.DB, lg.ID); err != nil {
			fmt.Printf("  [错误] 日志 %d 写台账失败: %v\n", lg.ID, err)
			continue
		}

		// 4. 调用运行时同一补录逻辑（幂等：仅追加 total_tokens - token_quota_synced 差额）
		model.ReconcileTaskTokensToQuotaData()
		processed++
		fmt.Printf("  [回填] 日志 %d 任务 %s total_tokens=%d\n", lg.ID, lg.TaskID, lg.Total)
	}

	fmt.Printf("  汇总：%s %d 条（跳过已应用 %d 条；任务不存在 %d 条）\n",
		map[bool]string{true: "回填", false: "待处理"}[*apply], processed, skipped, taskMissing)
}

// insertLedger 写入一条台账标记（防重复处理）。
func insertLedger(db *gorm.DB, logID int64) error {
	return db.Exec(fmt.Sprintf("INSERT INTO %s (log_id, applied_at) VALUES (?, ?)", ledgerTable), logID, time.Now().Unix()).Error
}

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [--check] [--apply] [--before N] [--task-id <id>] [--user-id N]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, strings.TrimSpace(`
修复异步任务"总 Token 数"历史累计数据（数据看板 quota_data.token_used）：
  扫描已回填 total_tokens 的异步任务提交日志，把 total_tokens 补到 task 行，
  并调用与运行时相同的补录逻辑（幂等）把差额累加进 quota_data.token_used。
默认 dry-run（--check），加 --apply 才写库；执行前请先备份（见输出提示）。
--apply 建议配合 --before（升级到新版运行时的 Unix 时间戳）或 --task-id 收窄范围，
避免重复处理升级后运行时已自动导出的任务。`))
		flag.PrintDefaults()
	}
}
