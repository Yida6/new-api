// fix_net_usage 一次性修复异步任务"概览消耗包含已退款预扣费"的历史累计数据。
//
// 背景：修复前，异步任务（Seedance/Suno/视频/MJ）提交时按预扣额累计 used_quota
// 并写入 quota_data（Count=1），而差额退款/全额退款只写退款日志、未冲减累计消耗，
// 也未写入 quota_data，导致概览"近24小时消耗/历史使用情况"显示毛预扣而非净消费。
//
// 本工具按退款日志（type=6）逐条冲减，使"净消费 = 累计消费 - 退款"成立：
//  1. users.used_quota    = 当前值 - SUM(未冲减的退款)（按用户）
//  2. channels.used_quota = 当前值 - SUM(未冲减的退款)（按渠道）
//  3. quota_data          = 为每条退款日志补写 Count=0、Quota=-退款额 的调整记录
//
// 关键设计（相对"按消费-退款全量重算"的旧方案）：
//   - 采用"差额冲减"而非"重算"：used_quota 只减去退款额，不再用日志反推消费额，
//     因此即使旧消费日志已被清理，已累计的消费也不会被抹掉（见 --before 与台账）。
//   - 单条退款在同一个事务内完成用户/渠道冲减 + quota_data 回填 + 台账写入，保证幂等。
//   - 默认 --dry-run 只报告差异，不写库（真正只读）；
//   - 升级到新运行时后，运行时已把退款写入 quota_data，迁移需用 --before 限定只处理
//     升级前的退款日志，避免重复冲减（台账只能识别"本工具处理过"的记录）。
//
// 安全措施：
//   - 执行前必须确认已完成备份（工具会打印备份命令）；
//   - quota_data 退款回填使用迁移台账表 net_usage_fix_applied 防重复应用；
//   - 冲减后净值为负的用户/渠道记录告警并钳制为 0，不制造负数。
//
// 用法：
//
//	go run ./migrate/fix_net_usage --dry-run --before <升级时间戳>
//	go run ./migrate/fix_net_usage --apply  --before <升级时间戳>
//
// 依赖环境变量（与主程序一致）：
//
//	SQL_DSN / LOG_SQL_DSN / SQLITE_PATH 等。
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
	apply     = flag.Bool("apply", false, "实际执行修复（默认 dry-run 只报告差异）")
	userID    = flag.Int("user-id", 0, "仅修复指定用户（0 = 全部）")
	channelID = flag.Int("channel-id", 0, "仅修复指定渠道（0 = 全部）")
	skipQuota = flag.Bool("skip-quota-data", false, "跳过 quota_data 退款回填（仅冲减 used_quota）")
	before    = flag.Int64("before", 0, "仅处理 created_at 早于该 Unix 时间戳的退款日志（设为升级到新版运行时的时刻，避免重复冲减运行时已导出的退款）")
	allowAll  = flag.Bool("allow-all", false, "危险：未指定 --before 时仍处理全部退款日志（可能重复冲减新版运行时已导出的退款）")
)

// 台账表带 scope 复合主键（log_id, scope），与旧版 net_usage_fix_applied（log_id 单主键、无 scope）
// schema 不兼容，故改用新表名避免 CREATE TABLE IF NOT EXISTS 命中旧表后 scope 查询/插入失败。
const ledgerTable = "net_usage_fix_applied_v2"

// 台账按 scope 区分两类幂等标记：used_quota（用户/渠道累计消耗冲减）与 quota_data（看板回填）。
// 分开记账使 --skip-quota-data 先跑 used_quota、之后正常重跑能单独补写 quota_data 而不会误判已处理。
const (
	scopeUsedQuota = "used_quota"
	scopeQuotaData = "quota_data"
)

const (
	logTypeConsume = 2
	logTypeRefund  = 6
)

type refundLog struct {
	ID        int64
	UserID    int
	Username  string
	ModelName string
	CreatedAt int64
	Quota     int
	Group     string
	TokenID   int
	ChannelID int
}

func main() {
	flag.Parse()

	// 未指定截止时间却要写库：无法区分"升级前需回填的退款"与"升级后运行时已导出的退款"，
	// 会重复冲减。要求显式 --before；确实要处理全部时用 --allow-all 明确承担风险。
	if *apply && *before == 0 && !*allowAll {
		fmt.Fprintln(os.Stderr, "错误：--apply 未指定 --before，将处理全部退款日志，可能重复冲减新版运行时已导出的退款。")
		fmt.Fprintln(os.Stderr, "请加 --before <升级时间戳>；若确要处理全部，请显式加 --allow-all。")
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

	fmt.Println("===== fix_net_usage: 修复异步任务净消费统计 =====")
	fmt.Printf("模式: %s | 主库: %s | 日志库: %s\n",
		map[bool]string{true: "APPLY（写库）", false: "DRY-RUN（只报告）"}[*apply],
		common.MainDatabaseType(), common.LogDatabaseType())
	if *userID > 0 {
		fmt.Printf("范围: 仅用户 %d\n", *userID)
	}
	if *channelID > 0 {
		fmt.Printf("范围: 仅渠道 %d\n", *channelID)
	}
	if *skipQuota {
		fmt.Println("范围: 跳过 quota_data 退款回填")
	}
	if *before > 0 {
		fmt.Printf("范围: 仅处理 created_at < %d 的退款日志\n", *before)
	} else if *allowAll {
		fmt.Println("范围: 处理全部退款日志（已显式 --allow-all）")
	}

	// 备份提示：无论 dry-run 还是 apply 都先打印
	printBackupInstructions()

	if !*apply {
		fmt.Println("\n当前为 DRY-RUN，不会写入任何数据。确认无误后加 --apply 执行。")
	}

	// 台账表仅在实际执行时创建，保证 dry-run 零写库
	if *apply {
		if err := ensureLedgerTable(); err != nil {
			fmt.Printf("初始化迁移台账表失败: %v\n", err)
			os.Exit(1)
		}
	}

	refunds, err := loadRefundLogs()
	if err != nil {
		fmt.Printf("查询退款日志失败: %v\n", err)
		os.Exit(1)
	}
	usedQuotaApplied := loadLedger(scopeUsedQuota)
	quotaDataApplied := loadLedger(scopeQuotaData)
	processRefunds(refunds, usedQuotaApplied, quotaDataApplied)

	fmt.Println("\n===== 完成 =====")
	if !*apply {
		fmt.Println("（DRY-RUN 模式，未写入任何数据）")
	}
}

// printBackupInstructions 打印执行前的备份方式。
func printBackupInstructions() {
	_, _ = os.Stdout.WriteString(`
【执行前必须备份】
  MySQL:
    mysqldump -h<HOST> -u<USER> -p<PASS> <DB> users channels quota_data logs > backup_net_usage_$(date +%Y%m%d%H%M%S).sql
  SQLite:
    cp <SQLITE_PATH>/one-api.db one-api.db.bak.$(date +%Y%m%d%H%M%S)
  PostgreSQL:
    pg_dump -h <HOST> -U <USER> -d <DB> -t users -t channels -t quota_data -t logs -F c -f backup_net_usage_$(date +%Y%m%d%H%M%S).dump
 建议：备份完成后先停机或暂停任务轮询（UPDATE_TASK=false 或停掉轮询系统任务），
       再执行 --apply，避免迁移期间新的预扣/退款写入造成并发差异。
 若新版运行时已上线：请用 --before <升级时间戳> 限定只处理升级前的退款日志。`)
}

// logGroupExpr 返回日志表 group 列的安全引用（Postgres 用双引号，其余用反引号）。
func logGroupExpr() string {
	if common.UsingLogDatabase(common.DatabaseTypePostgreSQL) {
		return `"group"`
	}
	return "`group`"
}

// ensureLedgerTable 创建迁移台账表（按 scope 区分 used_quota 与 quota_data 两类幂等标记）。
func ensureLedgerTable() error {
	return model.DB.Exec(fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (log_id BIGINT NOT NULL, scope VARCHAR(16) NOT NULL, applied_at BIGINT NOT NULL, PRIMARY KEY (log_id, scope))", ledgerTable)).Error
}

// loadRefundLogs 读取退款日志（按 id 升序保证确定性），并应用 --before 截止时间。
func loadRefundLogs() ([]refundLog, error) {
	query := fmt.Sprintf(
		"SELECT id, user_id, username, model_name, created_at, quota, %s, token_id, channel_id FROM logs WHERE type = %d",
		logGroupExpr(), logTypeRefund)
	if *before > 0 {
		query += fmt.Sprintf(" AND created_at < %d", *before)
	}
	query += " ORDER BY id"
	rows, err := model.LOG_DB.Raw(query).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []refundLog
	for rows.Next() {
		var r refundLog
		if err := rows.Scan(&r.ID, &r.UserID, &r.Username, &r.ModelName, &r.CreatedAt, &r.Quota, &r.Group, &r.TokenID, &r.ChannelID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadLedger 加载指定 scope 下已应用的退款日志 id 集合。
func loadLedger(scope string) map[int64]struct{} {
	out := make(map[int64]struct{})
	var ids []int64
	if err := model.DB.Table(ledgerTable).Where("scope = ?", scope).Pluck("log_id", &ids).Error; err != nil {
		if *apply {
			fmt.Printf("读取迁移台账失败: %v\n", err)
		}
		// dry-run 且台账表不存在属于预期，静默按空集合处理
		return out
	}
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

// insertLedger 在事务内写入一条 scope 幂等标记。
func insertLedger(tx *gorm.DB, logID int64, scope string) error {
	return tx.Exec(fmt.Sprintf("INSERT INTO %s (log_id, scope, applied_at) VALUES (?, ?, ?)", ledgerTable), logID, scope, time.Now().Unix()).Error
}

// ---------------------------------------------------------------------------
// 退款驱动的统一冲减：用户 used_quota + 渠道 used_quota + quota_data 回填 + 台账
// ---------------------------------------------------------------------------

func processRefunds(refunds []refundLog, usedQuotaApplied, quotaDataApplied map[int64]struct{}) {
	fmt.Println("\n--- 退款冲减：用户/渠道累计消耗 + quota_data 回填 ---")

	applied, skipped := 0, 0
	inserted, updated := 0, 0
	userClamped, channelClamped := 0, 0
	userTouched := map[int]int{}
	channelTouched := map[int]int64{}

	for _, r := range refunds {
		if *userID > 0 && r.UserID != *userID {
			continue
		}
		if *channelID > 0 && r.ChannelID != *channelID {
			continue
		}

		_, usedQuotaDone := usedQuotaApplied[r.ID]
		_, quotaDataDone := quotaDataApplied[r.ID]
		needQuotaData := !*skipQuota && !quotaDataDone
		if usedQuotaDone && !needQuotaData {
			skipped++
			continue
		}

		if !usedQuotaDone {
			userTouched[r.UserID] += r.Quota
			channelTouched[r.ChannelID] += int64(r.Quota)
		}

		if !*apply {
			fmt.Printf("  [待处理] 退款日志 %d（用户 %d，渠道 %d，额度 -%d）\n", r.ID, r.UserID, r.ChannelID, r.Quota)
			applied++
			continue
		}

		var clampedUser, clampedChannel bool
		var ins, upd int
		err := model.DB.Transaction(func(tx *gorm.DB) error {
			if !usedQuotaDone {
				// 用户累计消耗：减去退款额（不足则钳制为 0 并告警）
				c, err := decrementUserUsedQuota(tx, r.UserID, r.Quota)
				if err != nil {
					return err
				}
				clampedUser = c
				// 渠道累计消耗：减去退款额（不足则钳制为 0 并告警）
				c, err = decrementChannelUsedQuota(tx, r.ChannelID, int64(r.Quota))
				if err != nil {
					return err
				}
				clampedChannel = c
				if err := insertLedger(tx, r.ID, scopeUsedQuota); err != nil {
					return err
				}
			}
			if needQuotaData {
				i, u, _, err := backfillRefundQuotaData(tx, r)
				if err != nil {
					return err
				}
				ins, upd = i, u
				if err := insertLedger(tx, r.ID, scopeQuotaData); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			fmt.Printf("  [错误] 退款日志 %d 处理失败（事务已回滚）: %v\n", r.ID, err)
			continue
		}
		applied++
		inserted += ins
		updated += upd
		if clampedUser {
			userClamped++
			fmt.Printf("  [告警] 退款日志 %d 用户 %d 累计消耗不足，已钳制为 0（可能重复退款/日志缺失）\n", r.ID, r.UserID)
		}
		if clampedChannel {
			channelClamped++
			fmt.Printf("  [告警] 退款日志 %d 渠道 %d 累计消耗不足，已钳制为 0\n", r.ID, r.ChannelID)
		}
		fmt.Printf("  [回填] 退款日志 %d（用户 %d，渠道 %d，额度 -%d）\n", r.ID, r.UserID, r.ChannelID, r.Quota)
	}

	fmt.Printf("  汇总：%s %d 条退款日志（跳过已应用 %d 条），涉及用户 %d 个、渠道 %d 个，累计退款额度 %d\n",
		map[bool]string{true: "回填", false: "待处理"}[*apply], applied, skipped, len(userTouched), len(channelTouched), sumRefunds(channelTouched))
	if *apply {
		fmt.Printf("        quota_data：新增 %d 行、冲减 %d 行；钳制：用户 %d 例、渠道 %d 例\n", inserted, updated, userClamped, channelClamped)
	}
}

func sumRefunds(m map[int]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}

// decrementUserUsedQuota 在事务内将用户 used_quota 减去 refund；不足时钳制为 0 并返回 clamped=true。
func decrementUserUsedQuota(tx *gorm.DB, userId int, refund int) (clamped bool, err error) {
	var cur int
	if err := tx.Model(&model.User{}).Where("id = ?", userId).Select("used_quota").Scan(&cur).Error; err != nil {
		return false, err
	}
	next := cur - refund
	if next < 0 {
		clamped = true
		next = 0
	}
	if next == cur {
		return clamped, nil
	}
	err = tx.Model(&model.User{}).Where("id = ?", userId).Update("used_quota", next).Error
	return clamped, err
}

// decrementChannelUsedQuota 在事务内将渠道 used_quota 减去 refund；不足时钳制为 0 并返回 clamped=true。
func decrementChannelUsedQuota(tx *gorm.DB, channelId int, refund int64) (clamped bool, err error) {
	var cur int64
	if err := tx.Model(&model.Channel{}).Where("id = ?", channelId).Select("used_quota").Scan(&cur).Error; err != nil {
		return false, err
	}
	next := cur - refund
	if next < 0 {
		clamped = true
		next = 0
	}
	if next == cur {
		return clamped, nil
	}
	err = tx.Model(&model.Channel{}).Where("id = ?", channelId).Update("used_quota", next).Error
	return clamped, err
}

// backfillRefundQuotaData 在事务内为单条退款日志回填 quota_data 调整记录。
// 返回 (新增行数, 冲减行数, 多节点冲减次数)。
func backfillRefundQuotaData(tx *gorm.DB, r refundLog) (inserted, updated, multi int, err error) {
	hourBucket := r.CreatedAt - (r.CreatedAt % 3600)
	var matches []model.QuotaData
	q := tx.Table("quota_data").
		Where("user_id = ? AND username = ? AND model_name = ? AND created_at = ? AND use_group = ? AND token_id = ? AND channel_id = ?",
			r.UserID, r.Username, r.ModelName, hourBucket, r.Group, r.TokenID, r.ChannelID)
	if err := q.Order("id ASC").Find(&matches).Error; err != nil {
		return 0, 0, 0, err
	}

	switch len(matches) {
	case 0:
		// 无对应消费行（例如提交日志不在本库/已被清理）：补一条调整行
		row := &model.QuotaData{
			UserID:    r.UserID,
			Username:  r.Username,
			ModelName: r.ModelName,
			CreatedAt: hourBucket,
			UseGroup:  r.Group,
			TokenID:   r.TokenID,
			ChannelID: r.ChannelID,
			NodeName:  "",
			TokenUsed: 0,
			Count:     0,
			Quota:     -r.Quota,
		}
		if err := tx.Table("quota_data").Create(row).Error; err != nil {
			return 0, 0, 0, err
		}
		return 1, 0, 0, nil
	case 1:
		// 唯一匹配（通常即提交预扣行）：直接冲减，保持节点归属
		if err := tx.Table("quota_data").Where("id = ?", matches[0].Id).
			Update("quota", gorm.Expr("quota - ?", r.Quota)).Error; err != nil {
			return 0, 0, 0, err
		}
		return 0, 1, 0, nil
	default:
		// 多行匹配（多节点同 key 拆分）：冲减最小 id 行，聚合口径仍正确
		fmt.Printf("  [提示] 退款日志 %d 命中 %d 行（多节点拆分），冲减最小 id 行\n", r.ID, len(matches))
		if err := tx.Table("quota_data").Where("id = ?", matches[0].Id).
			Update("quota", gorm.Expr("quota - ?", r.Quota)).Error; err != nil {
			return 0, 0, 0, err
		}
		return 0, 1, 1, nil
	}
}

func init() {
	// 保证 flag 帮助信息可读
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [--apply] [--before N] [--allow-all] [--user-id N] [--channel-id N] [--skip-quota-data]\n", os.Args[0])
		fmt.Fprintln(os.Stderr, strings.TrimSpace(`
修复异步任务净消费统计的历史累计数据：
  users.used_quota / channels.used_quota 按"退款日志"差额冲减（不重算消费，避免丢失已清理日志）；
  quota_data 为退款日志补写 Count=0、Quota=-退款额 的调整记录。
默认 dry-run，加 --apply 才写库；执行前请先备份（见输出提示）。
--apply 必须配合 --before（升级到新版运行时的 Unix 时间戳），避免重复冲减运行时已导出的退款；
确要处理全部退款时用 --allow-all 显式承担风险。`))
		flag.PrintDefaults()
	}
}
