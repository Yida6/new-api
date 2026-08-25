package model

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// QuotaData 柱状图数据
type QuotaData struct {
	Id        int    `json:"id"`
	UserID    int    `json:"user_id" gorm:"index"`
	Username  string `json:"username" gorm:"index:idx_qdt_model_user_name,priority:2;size:64;default:''"`
	ModelName string `json:"model_name" gorm:"index:idx_qdt_model_user_name,priority:1;size:64;default:''"`
	CreatedAt int64  `json:"created_at" gorm:"bigint;index:idx_qdt_created_at,priority:2"`
	UseGroup  string `json:"use_group" gorm:"index;size:64;default:''"`
	TokenID   int    `json:"token_id" gorm:"index;default:0"`
	ChannelID int    `json:"channel_id" gorm:"index;default:0"`
	NodeName  string `json:"node_name" gorm:"index;size:64;default:''"`
	TokenUsed int    `json:"token_used" gorm:"default:0"`
	Count     int    `json:"count" gorm:"default:0"`
	Quota     int64  `json:"quota" gorm:"type:bigint;default:0"`
}

type QuotaDataLogParams struct {
	UserID    int
	Username  string
	ModelName string
	Quota     int64
	CreatedAt int64
	TokenUsed int
	UseGroup  string
	TokenID   int
	ChannelID int
	NodeName  string
	// Adjustment 为 true 时表示一条计费调整记录（差额补扣/差额退款/全额退款）：
	// 写入 quota_data 时 Count 固定为 0（不增加请求计数），Quota 可为负数。
	// 用于保证"净消费 = 消费日志合计 - 退款日志合计"在看板趋势上成立。
	Adjustment bool
}

func UpdateQuotaData() {
	for {
		if common.DataExportEnabled {
			common.SysLog("正在更新数据看板数据...")
			SaveQuotaDataCache()
		}
		time.Sleep(time.Duration(common.DataExportInterval) * time.Minute)
	}
}

var CacheQuotaData = make(map[string]*QuotaData)
var CacheQuotaDataLock = sync.Mutex{}

func logQuotaDataCache(quotaData *QuotaData) {
	key := fmt.Sprintf("%d\x00%s\x00%s\x00%d\x00%s\x00%d\x00%d\x00%s",
		quotaData.UserID,
		quotaData.Username,
		quotaData.ModelName,
		quotaData.CreatedAt,
		quotaData.UseGroup,
		quotaData.TokenID,
		quotaData.ChannelID,
		quotaData.NodeName,
	)
	count := quotaData.Count
	quota := quotaData.Quota
	tokenUsed := quotaData.TokenUsed
	cachedQuotaData, ok := CacheQuotaData[key]
	if ok {
		cachedQuotaData.Count += count
		cachedQuotaData.Quota += quota
		cachedQuotaData.TokenUsed += tokenUsed
		quotaData = cachedQuotaData
	}
	CacheQuotaData[key] = quotaData
}

func LogQuotaData(params QuotaDataLogParams) {
	// 只精确到小时
	createdAt := params.CreatedAt - (params.CreatedAt % 3600)
	count := 1
	if params.Adjustment {
		// 差额/退款调整不增加请求计数
		count = 0
	}
	quotaData := &QuotaData{
		UserID:    params.UserID,
		Username:  params.Username,
		ModelName: params.ModelName,
		CreatedAt: createdAt,
		UseGroup:  params.UseGroup,
		TokenID:   params.TokenID,
		ChannelID: params.ChannelID,
		NodeName:  params.NodeName,
		Count:     count,
		Quota:     params.Quota,
		TokenUsed: params.TokenUsed,
	}

	CacheQuotaDataLock.Lock()
	defer CacheQuotaDataLock.Unlock()
	logQuotaDataCache(quotaData)
}

func SaveQuotaDataCache() {
	CacheQuotaDataLock.Lock()
	size := len(CacheQuotaData)
	// 如果缓存中有数据，就保存到数据库中
	// 1. 先查询数据库中是否有数据
	// 2. 如果有数据，就更新数据
	// 3. 如果没有数据，就插入数据
	for _, quotaData := range CacheQuotaData {
		quotaDataDB := &QuotaData{}
		DB.Table("quota_data").
			Where("user_id = ? and username = ? and model_name = ? and created_at = ? and use_group = ? and token_id = ? and channel_id = ? and node_name = ?",
				quotaData.UserID, quotaData.Username, quotaData.ModelName, quotaData.CreatedAt, quotaData.UseGroup, quotaData.TokenID, quotaData.ChannelID, quotaData.NodeName).
			First(quotaDataDB)
		if quotaDataDB.Id > 0 {
			increaseQuotaData(quotaData)
		} else {
			DB.Table("quota_data").Create(quotaData)
		}
	}
	CacheQuotaData = make(map[string]*QuotaData)
	// 缓存清空/交换后立即释放全局锁，避免把下方异步任务 token 补录（可能扫描大量
	// 任务 + 逐任务事务）也锁在同一把锁内，导致新请求的 LogQuotaData 长时间被阻塞。
	CacheQuotaDataLock.Unlock()
	common.SysLog(fmt.Sprintf("保存数据看板数据成功，共保存%d条数据", size))
	// 缓存刷盘后执行异步任务 token 补录：把已持久化 total_tokens 但尚未回填
	// 的已完成任务补进 quota_data.token_used（幂等，见 ReconcileTaskTokensToQuotaData）。
	ReconcileTaskTokensToQuotaData()
}

func increaseQuotaData(quotaData *QuotaData) {
	err := DB.Table("quota_data").
		Where("user_id = ? and username = ? and model_name = ? and created_at = ? and use_group = ? and token_id = ? and channel_id = ? and node_name = ?",
			quotaData.UserID, quotaData.Username, quotaData.ModelName, quotaData.CreatedAt, quotaData.UseGroup, quotaData.TokenID, quotaData.ChannelID, quotaData.NodeName).
		Updates(map[string]interface{}{
			"count":      gorm.Expr("count + ?", quotaData.Count),
			"quota":      gorm.Expr("quota + ?", quotaData.Quota),
			"token_used": gorm.Expr("token_used + ?", quotaData.TokenUsed),
		}).Error
	if err != nil {
		common.SysLog(fmt.Sprintf("increaseQuotaData error: %s", err))
	}
}

func GetQuotaDataByUsername(username string, startTime int64, endTime int64) (quotaData []*QuotaData, err error) {
	var quotaDatas []*QuotaData
	// 从quota_data表中查询数据
	err = DB.Table("quota_data").
		Select("user_id, username, model_name, created_at, sum(count) as count, sum(quota) as quota, sum(token_used) as token_used").
		Where("username = ? and created_at >= ? and created_at <= ?", username, startTime, endTime).
		Group("user_id, username, model_name, created_at").
		Find(&quotaDatas).Error
	return quotaDatas, err
}

func GetQuotaDataByUserId(userId int, startTime int64, endTime int64) (quotaData []*QuotaData, err error) {
	var quotaDatas []*QuotaData
	// 从quota_data表中查询数据
	err = DB.Table("quota_data").
		Select("user_id, username, model_name, created_at, sum(count) as count, sum(quota) as quota, sum(token_used) as token_used").
		Where("user_id = ? and created_at >= ? and created_at <= ?", userId, startTime, endTime).
		Group("user_id, username, model_name, created_at").
		Find(&quotaDatas).Error
	return quotaDatas, err
}

func GetQuotaDataGroupByUser(startTime int64, endTime int64) (quotaData []*QuotaData, err error) {
	var quotaDatas []*QuotaData
	err = DB.Table("quota_data").
		Select("username, created_at, sum(count) as count, sum(quota) as quota, sum(token_used) as token_used").
		Where("created_at >= ? and created_at <= ?", startTime, endTime).
		Group("username, created_at").
		Find(&quotaDatas).Error
	return quotaDatas, err
}

func GetAllQuotaDates(startTime int64, endTime int64, username string) (quotaData []*QuotaData, err error) {
	if username != "" {
		return GetQuotaDataByUsername(username, startTime, endTime)
	}
	var quotaDatas []*QuotaData
	// 从quota_data表中查询数据
	// only select model_name, sum(count) as count, sum(quota) as quota, model_name, created_at from quota_data group by model_name, created_at;
	//err = DB.Table("quota_data").Where("created_at >= ? and created_at <= ?", startTime, endTime).Find(&quotaDatas).Error
	err = DB.Table("quota_data").Select("model_name, sum(count) as count, sum(quota) as quota, sum(token_used) as token_used, created_at").Where("created_at >= ? and created_at <= ?", startTime, endTime).Group("model_name, created_at").Find(&quotaDatas).Error
	return quotaDatas, err
}

// ---------------------------------------------------------------------------
// 异步任务实际 token 计入数据看板（quota_data.token_used）
// ---------------------------------------------------------------------------
// 背景：异步任务（Seedance 等）提交瞬间上游无 usage，提交消费日志写
// quota_data 时 TokenUsed=0；结算完成后才拿到实际 total_tokens。此前只把
// total_tokens 回填到 logs.other.total_tokens，看板（读 quota_data.token_used）
// 仍为 0。
//
// 设计（与计费差额完全解耦）：
//  1. 结算时 RecordTaskTotalTokensToQuotaData 仅把 TotalTokens 持久化到
//     task 行（durable、幂等），不直接写 quota_data——避免与缓存刷盘重复累计；
//  2. 后台 ReconcileTaskTokensToQuotaData（随 SaveQuotaDataCache 定时/停机时
//     触发）扫描 "total_tokens > 0 且 token_quota_synced < total_tokens" 的任务，
//     在事务内只把正差额 (total_tokens - token_quota_synced) 累加到对应小时桶
//     （Count=0），并把 token_quota_synced 更新为 total_tokens；
//  3. 事务保证"写 quota_data + 更新 token_quota_synced"原子（值差额 + CAS），
//     重复扫描/重复结算/服务重启均不会重复累计，旧值更正为新值只追加差额；
//     TotalTokens 持久化到 task 行后即使进程崩溃，下轮 flush 也能补回，不丢失
//     "任务先完成、提交统计尚未刷盘"的窗口。

// RecordTaskTotalTokensToQuotaData 结算拿到上游实际 total_tokens 后，将任务行
// 的 TotalTokens 持久化（durable），作为后续 quota_data 补录的数据源。
// 仅当 DataExportEnabled 且 total_tokens > 0 才写入；不在此处改 quota_data，
// 保证与计费差额（补扣/退款/预扣命中）解耦。
//
// 返回 (ok, err)：ok 表示本次持久化成功（或属可忽略的幂等跳过：total_tokens<=0
// 或 DataExportEnabled=false，此时 err=nil 且 ok=false）；err 非 nil 仅当确实是
// 数据库写入失败。调用方应把 err 纳入结算失败可重试语义，避免 token 统计静默丢失；
// 而 ok=false、err=nil 的跳过属预期，绝不阻断任务进入终态。
func RecordTaskTotalTokensToQuotaData(task *Task, totalTokens int) (bool, error) {
	if task == nil || task.ID == 0 || totalTokens <= 0 {
		return false, nil
	}
	if !common.DataExportEnabled {
		return false, nil
	}
	err := DB.Model(&Task{}).Where("id = ?", task.ID).
		UpdateColumn("total_tokens", totalTokens).Error
	if err != nil {
		common.SysError("record task total_tokens error: " + err.Error())
		return false, err
	}
	task.TotalTokens = totalTokens
	return true, nil
}

// ReconcileTaskTokensToQuotaData 把已持久化 total_tokens 但尚未完全回填
// quota_data 的已完成任务补录进数据看板。随 SaveQuotaDataCache 定时调用
// （停机时也会在 main 退出路径调用一次），可重复执行、幂等。
//
// 幂等（值差额语义）：每个任务在"写 quota_data + 更新 token_quota_synced"同一
// 事务内完成，只累计 (total_tokens - token_quota_synced) 的正差额；已同步同值
// 跳过、从未同步补全量、旧值更正为新值只追加差额。重复执行/重复结算/服务重启
// 不会重复累计 total_tokens，也不会改变 quota_data 的 count/quota 统计。
// 返回本次补录的任务数。
func ReconcileTaskTokensToQuotaData() int {
	if !common.DataExportEnabled {
		return 0
	}
	total := 0
	total += reconcileRecordedTasks()
	total += recoverTasksFromData()
	// 日志兜底通道：task.Data 无 usage（上游响应未含 usage 或结构差异）的
	// 存量任务，其 total_tokens 可能只存在于日志 other.total_tokens（结算/提交
	// 日志）。补一道读日志回填，确保这类历史记录也能恢复，不依赖 task.Data。
	total += recoverTasksFromLogs()
	if total > 0 {
		common.SysLog(fmt.Sprintf("数据看板 Token 补录完成：共 %d 条异步任务", total))
	}
	return total
}

// reconcileRecordedTasks 扫描"已持久化 total_tokens 且尚未完全同步"的任务并补录。
// 按主键分批，避免一次加载过多任务。
func reconcileRecordedTasks() int {
	const batchSize = 200
	total := 0
	var lastID int64
	for {
		var tasks []*Task
		err := DB.Where("total_tokens > 0 AND token_quota_synced < total_tokens").
			Where("id > ?", lastID).Order("id").
			Limit(batchSize).Find(&tasks).Error
		if err != nil {
			common.SysError("reconcile task tokens query error: " + err.Error())
			break
		}
		if len(tasks) == 0 {
			break
		}
		for _, t := range tasks {
			if reconcileTaskToken(t) {
				total++
			}
			lastID = t.ID
		}
		if len(tasks) < batchSize {
			break
		}
	}
	return total
}

// recoverTasksFromData 自愈存量异步任务：结算升级前创建的 Seedance 等任务，其
// tasks.total_tokens 恒为 0（字段迁移默认值），但原始上游响应已存在 task.Data
// （含 usage.total_tokens）。补录器无法靠"已持久化 total_tokens"识别它们，故此处
// 额外扫描"终态成功且 total_tokens=0 且从未同步（token_quota_synced=0）"的任务，
// 从 task.Data 解析出实际 total_tokens，持久化后立即在同一趟内调用
// reconcileTaskToken 补录进 quota_data（闭环，不等下一轮），并计入返回值。
//
// 自愈窗口：每个任务只在首次成功恢复后被移出本集合；task.Data 无 usage（无 token）
// 的残余任务会在此保留，但数量小且稳定，逐轮重读代价可忽略。task.Data 无 usage 而
// total_tokens 只存在于日志 other.total_tokens 的历史任务，由紧随其后的
// recoverTasksFromLogs 兜底恢复（见 ReconcileTaskTokensToQuotaData 第三通道）。
func recoverTasksFromData() int {
	const batchSize = 200
	total := 0
	var lastID int64
	for {
		var tasks []*Task
		err := DB.Where("status = ? AND total_tokens = 0 AND token_quota_synced = 0", TaskStatusSuccess).
			Where("id > ?", lastID).Order("id").
			Limit(batchSize).Find(&tasks).Error
		if err != nil {
			common.SysError("recover task tokens from data query error: " + err.Error())
			break
		}
		if len(tasks) == 0 {
			break
		}
		for _, t := range tasks {
			if tt := extractTaskDataTotalTokens(t); tt > 0 {
				if ok, perr := RecordTaskTotalTokensToQuotaData(t, tt); ok {
					// 持久化成功后立即补录进 quota_data（同一次扫带内完成，
					// 避免本任务遗留到下一轮才累计）。
					if reconcileTaskToken(t) {
						total++
					}
				} else if perr != nil {
					// 写库失败：本轮跳过，下轮重试（自愈）。
					common.SysError(fmt.Sprintf("recover task %s total_tokens persist failed: %v", t.TaskID, perr))
				}
			}
			lastID = t.ID
		}
		if len(tasks) < batchSize {
			break
		}
	}
	return total
}

// recoverTasksFromLogs 自愈存量任务的第二兜底通道：针对 task.Data 无 usage
// （recoverTasksFromData 无能为力）的终态成功任务，从该任务的日志
// other.total_tokens 回填 tasks.total_tokens 并补录进 quota_data。
//
// 数据源：结算路径把上游实际 total_tokens 写入任务结算/调整日志
// （RecordTaskBillingLog 的 other.total_tokens）与提交消费日志
// （BackfillTaskConsumeLogTotalTokens），二者均携带 other.task_id 与任务
// TaskID 一致。此处按 task_id 定位日志并读取 total_tokens，是 task.Data 之外
// 唯一能恢复历史统计的可靠来源。
//
// 幂等：与 recoverTasksFromData 共用同一扫描条件
// （status=success AND total_tokens=0 AND token_quota_synced=0），且成功恢复后
// token_quota_synced 置为 total_tokens，任务即移出本集合，重复执行不重复累计。
// 无日志/无 total_tokens 的残余任务保留在此，数量小且稳定，逐轮重读代价可忽略。
// 返回本次恢复的任务数。
func recoverTasksFromLogs() int {
	const batchSize = 200
	total := 0
	var lastID int64
	for {
		var tasks []*Task
		err := DB.Where("status = ? AND total_tokens = 0 AND token_quota_synced = 0", TaskStatusSuccess).
			Where("id > ?", lastID).Order("id").
			Limit(batchSize).Find(&tasks).Error
		if err != nil {
			common.SysError("recover task tokens from logs query error: " + err.Error())
			break
		}
		if len(tasks) == 0 {
			break
		}
		for _, t := range tasks {
			if tt := findTaskTotalTokensFromLogs(t.UserId, t.TaskID); tt > 0 {
				if ok, perr := RecordTaskTotalTokensToQuotaData(t, tt); ok {
					// 持久化成功后立即补录进 quota_data（同一次扫描内闭环）。
					if reconcileTaskToken(t) {
						total++
					}
				} else if perr != nil {
					// 写库失败：本轮跳过，下轮重试（自愈）。
					common.SysError(fmt.Sprintf("recover task %s total_tokens from log persist failed: %v", t.TaskID, perr))
				}
			}
			lastID = t.ID
		}
		if len(tasks) < batchSize {
			break
		}
	}
	return total
}

// findTaskTotalTokensFromLogs 从任务的日志 other.total_tokens 解析实际 token 总量。
// 覆盖消费日志（type=consume，含 is_task 标记的提交日志）与调整日志（补扣
// consume / 退款 refund），它们都通过 other.task_id 关联任务并写入 total_tokens。
// 任一匹配日志含 >0 的 total_tokens 即返回（取 id 倒序第一条有效值）。
// 找不到/无 total_tokens/解析失败返回 0（不写入伪造数据）。仅读日志，不落库。
func findTaskTotalTokensFromLogs(userId int, taskID string) int {
	if userId <= 0 || taskID == "" {
		return 0
	}
	// LIKE 拆分匹配：`%"total_tokens":%` 筛出含 token 字段的日志，
	// `%<taskID>%` 粗筛 task_id 可能匹配的候选行（避免与 BackfillTaskConsumeLog
	// 同款 glebarez/sqlite 末尾 `%"` 匹配异常，见 findTaskSubmitLog 注释）。
	pattern := "%" + taskID + "%"
	var logs []*Log
	err := LOG_DB.Where("user_id = ? AND (type = ? OR type = ?) AND other LIKE ? AND other LIKE ?",
		userId, LogTypeConsume, LogTypeRefund, `%"total_tokens":%`, pattern).
		Order("id desc").Limit(20).Find(&logs).Error
	if err != nil {
		common.SysError("find task tokens from logs query error: " + err.Error())
		return 0
	}
	for _, lg := range logs {
		other, _ := common.StrToMap(lg.Other)
		if other == nil {
			continue
		}
		// 粗筛只用了 taskID 子串，需精确校验 other.task_id 与目标一致，防止
		// 其他任务日志因 ID 前缀相同而误读。
		if tid, ok := other["task_id"].(string); ok && tid != taskID {
			continue
		}
		if v, ok := other["total_tokens"].(float64); ok && v > 0 {
			return int(v)
		}
	}
	return 0
}

// extractTaskDataTotalTokens 从任务原始上游响应 task.Data 解析 usage.total_tokens。
// 解析失败或缺失返回 0（不写入伪造数据）。仅做内存解析，不落库、不写日志。
func extractTaskDataTotalTokens(task *Task) int {
	if task == nil || len(task.Data) == 0 {
		return 0
	}
	var d struct {
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := common.Unmarshal(task.Data, &d); err != nil {
		return 0
	}
	if d.Usage.TotalTokens <= 0 {
		return 0
	}
	return d.Usage.TotalTokens
}

// reconcileTaskToken 在事务内为单个任务把 (total_tokens - token_quota_synced) 的
// 正差额累加到对应小时桶，并把 token_quota_synced 更新为 total_tokens。事务保证
// 原子性：任一步失败整体回滚，下轮可重跑；值差额语义保证已同步的部分不重复累计。
// 返回 true 表示本次实际累计了差额。
func reconcileTaskToken(task *Task) bool {
	if task == nil || task.ID <= 0 || task.TotalTokens <= 0 {
		return false
	}
	// username 通过全局 DB 查询（GetUsernameById 内部可能回源 Redis/DB），
	// 必须在事务外完成：测试/单连接池环境下，事务会独占唯一连接，事务内再取
	// 连接会死锁。桶维度需要与提交消费日志写入 quota_data 时的 username 一致。
	username := ""
	if u, err := GetUsernameById(task.UserId, false); err == nil {
		username = u
	}
	hourBucket := task.SubmitTime - (task.SubmitTime % 3600)
	if hourBucket <= 0 {
		hourBucket = task.CreatedAt - (task.CreatedAt % 3600)
	}
	ok := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 1. 读取当前已同步值（行锁/事务内串行），并计算待补差额。
		var cur Task
		if err := tx.Model(&Task{}).Select("token_quota_synced").Where("id = ?", task.ID).First(&cur).Error; err != nil {
			return err
		}
		delta := task.TotalTokens - cur.TokenQuotaSynced
		if delta <= 0 {
			// 已同步同值或新值更小（不写负 Token）：幂等跳过。
			return nil
		}
		// 2. 幂等置位（CAS 抢占）：仅当 token_quota_synced 仍等于读取时的值才更新为
		//    total_tokens，防并发/重复调用双计；R 影响 0 行说明被其他流程抢先。
		claim := tx.Model(&Task{}).Where("id = ? AND token_quota_synced = ?", task.ID, cur.TokenQuotaSynced).
			UpdateColumn("token_quota_synced", task.TotalTokens)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			// 已被其他流程更新：跳过，不重复累计。
			return nil
		}
		// 3. 把差额累计进对应小时桶（Count=0，不增加请求计数）。
		if err := incrementQuotaDataToken(tx, task, username, hourBucket, delta); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		common.SysError(fmt.Sprintf("reconcile task token error (task=%s): %v", task.TaskID, err))
		return false
	}
	return ok
}

// incrementQuotaDataToken 把 tokens 累加到指定小时桶的 token_used（Count 固定 0）。
// 兼容 SQLite / MySQL / PostgreSQL：不依赖 JSON 函数；桶不存在则插入调整行，
// 存在则增量更新。在调用方事务内执行以保证原子。
func incrementQuotaDataToken(tx *gorm.DB, task *Task, username string, hourBucket int64, tokens int) error {
	conditions := map[string]interface{}{
		"user_id":    task.UserId,
		"username":   username,
		"model_name": taskModelNameForQuotaData(task),
		"created_at": hourBucket,
		"use_group":  task.Group,
		"token_id":   task.PrivateData.TokenId,
		"channel_id": task.ChannelId,
		"node_name":  task.PrivateData.NodeName,
	}
	// 定位已有桶行（多节点同 key 拆分时取最小 id，聚合口径仍正确）。
	var target QuotaData
	err := tx.Table("quota_data").
		Where("user_id = ? AND username = ? AND model_name = ? AND created_at = ? AND use_group = ? AND token_id = ? AND channel_id = ? AND node_name = ?",
			conditions["user_id"], conditions["username"], conditions["model_name"], conditions["created_at"],
			conditions["use_group"], conditions["token_id"], conditions["channel_id"], conditions["node_name"]).
		Order("id ASC").Limit(1).First(&target).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 桶行不存在（提交消费日志可能尚未刷盘）：插入一条纯 Token 调整行，
		// 后续缓存刷盘会叠加 Count/Quota，聚合结果仍正确。
		row := &QuotaData{
			UserID:    task.UserId,
			Username:  username,
			ModelName: taskModelNameForQuotaData(task),
			CreatedAt: hourBucket,
			UseGroup:  task.Group,
			TokenID:   task.PrivateData.TokenId,
			ChannelID: task.ChannelId,
			NodeName:  task.PrivateData.NodeName,
			TokenUsed: tokens,
			Count:     0,
			Quota:     0,
		}
		return tx.Table("quota_data").Create(row).Error
	}
	return tx.Table("quota_data").Where("id = ?", target.Id).
		Update("token_used", gorm.Expr("token_used + ?", tokens)).Error
}

// taskModelNameForQuotaData 返回任务归属模型名（与提交消费日志写入 quota_data
// 时使用的 ModelName 一致，保证命中同一小时桶）。
func taskModelNameForQuotaData(task *Task) string {
	if task == nil {
		return ""
	}
	if task.Properties.OriginModelName != "" {
		return task.Properties.OriginModelName
	}
	return ""
}
