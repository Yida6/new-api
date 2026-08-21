package controller

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ============================================================================
// 大额度（int64）回归：11111 / 111111 USD 有限额度 API Key
// QuotaPerUnit 默认 500000，因此：
//   11111  USD → 5,555,500,000 内部额度
//   111111 USD → 55,555,500,000 内部额度
// 旧实现被 int32 上限（≈4294.96 USD）压死，int64 化后必须可创建/更新。
// ============================================================================

const (
	largeQuota11111  = int64(11111 * 500000)   // 5,555,500,000
	largeQuota111111 = int64(111111 * 500000)  // 55,555,500,000
	largeUserBalance = int64(200000 * 500000)  // 钱包余额 20 万美元额度
)

func setupLargeQuotaUser(t *testing.T, id int) *model.User {
	t.Helper()
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.User{}))
	user := &model.User{
		Id:       id,
		Username: "large-quota-user",
		Password: "password",
		Group:    "default",
		Status:   common.UserStatusEnabled,
		Quota:    largeUserBalance,
	}
	require.NoError(t, db.Create(user).Error)
	return user
}

func TestAddTokenLargeFiniteQuota11111USD(t *testing.T) {
	user := setupLargeQuotaUser(t, 301)
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/",
		quotaTokenRequest("large-11111", int(largeQuota11111), false), user.Id)
	AddToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, "11111 USD finite quota must be accepted: %s", response.Message)
	// 落库校验：RemainQuota 为 int64 大值
	var saved model.Token
	require.NoError(t, model.DB.Where("name = ?", "large-11111").First(&saved).Error)
	require.Equal(t, largeQuota11111, saved.RemainQuota)
}

func TestAddTokenLargeFiniteQuota111111USD(t *testing.T) {
	user := setupLargeQuotaUser(t, 302)
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/",
		quotaTokenRequest("large-111111", int(largeQuota111111), false), user.Id)
	AddToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, "111111 USD finite quota must be accepted: %s", response.Message)
	var saved model.Token
	require.NoError(t, model.DB.Where("name = ?", "large-111111").First(&saved).Error)
	require.Equal(t, largeQuota111111, saved.RemainQuota)
}

func TestUpdateTokenLargeFiniteQuotaRoundTrip(t *testing.T) {
	user := setupLargeQuotaUser(t, 303)
	token := seedToken(t, model.DB, user.Id, "roundtrip", "roundtrip1234567890")

	// 编辑：额度 111111 USD → 11111 USD
	body := map[string]any{
		"id":              token.Id,
		"name":            "roundtrip",
		"expired_time":    -1,
		"remain_quota":    int(largeQuota11111),
		"unlimited_quota": false,
		"group":           "default",
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/token/", body, user.Id)
	UpdateToken(ctx)
	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)

	var saved model.Token
	require.NoError(t, model.DB.Where("id = ?", token.Id).First(&saved).Error)
	require.Equal(t, largeQuota11111, saved.RemainQuota, "编辑后额度与提交往返一致")

	// 再编辑回 111111 USD
	body["remain_quota"] = int(largeQuota111111)
	ctx, recorder = newAuthenticatedContext(t, http.MethodPut, "/api/token/", body, user.Id)
	UpdateToken(ctx)
	response = decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
	require.NoError(t, model.DB.Where("id = ?", token.Id).First(&saved).Error)
	require.Equal(t, largeQuota111111, saved.RemainQuota, "编辑往返一致")
}

func TestAddTokenQuotaAboveBusinessCeilingRejected(t *testing.T) {
	user := setupLargeQuotaUser(t, 304)
	// common.MaxTokenQuota + 1：业务上限之外，后端必须明确拒绝（即使钱包余额充足）。
	above := common.MaxTokenQuota + 1
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/",
		quotaTokenRequest("above-ceiling", int(above), false), user.Id)
	AddToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.False(t, response.Success, "quota above MaxTokenQuota must be rejected")
	// 测试环境未加载 i18n，message 为 key；生产环境为中文"额度值超出有效范围"
	require.Contains(t, response.Message, "quota_exceed_max")
}

func TestAddTokenSmallQuotaUnchanged(t *testing.T) {
	user := setupLargeQuotaUser(t, 305)
	// 既有小额度行为不变：100 额度有限 Key 照常创建
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/",
		quotaTokenRequest("small", 100, false), user.Id)
	AddToken(ctx)
	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)

	var saved model.Token
	require.NoError(t, model.DB.Where("name = ?", "small").First(&saved).Error)
	require.Equal(t, int64(100), saved.RemainQuota)
}

// ============================================================================
// BIGINT 迁移：旧 int 列 → 新 int64 模型，保留数据、列类型升级为 BIGINT。
// SQLite 必跑（INTEGER 原生 64 位，无需改列但数据保留）；MySQL/PG 通过
// TEST_MYSQL_DSN / TEST_POSTGRES_DSN 环境变量条件运行（无 DSN 时跳过）。
// ============================================================================

// legacyQuotaModel 模拟升级前的 int32 额度模型（旧 gorm tag）。
type legacyQuotaModel struct {
	Id         int `gorm:"primaryKey"`
	Username   string
	Quota      int `gorm:"type:int;default:0"`
	UsedQuota  int `gorm:"type:int;default:0"`
	AffQuota   int `gorm:"type:int;default:0"`
	AffHistory int `gorm:"column:aff_history;type:int;default:0"`
}

func (legacyQuotaModel) TableName() string { return "quota_mig_test" }

// newQuotaModel 升级后的 int64 模型（与 model.User 的额度列同一 gorm tag 口径）。
type newQuotaModel struct {
	Id         int `gorm:"primaryKey"`
	Username   string
	Quota      int64 `gorm:"type:bigint;default:0"`
	UsedQuota  int64 `gorm:"type:bigint;default:0"`
	AffQuota   int64 `gorm:"type:bigint;default:0"`
	AffHistory int64 `gorm:"column:aff_history;type:bigint;default:0"`
}

func (newQuotaModel) TableName() string { return "quota_mig_test" }

func openQuotaMigrationDB(t *testing.T, dialect, dsn string) *gorm.DB {
	t.Helper()
	var db *gorm.DB
	var err error
	switch dialect {
	case "sqlite":
		db, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	case "mysql":
		db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	case "postgres":
		db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	default:
		t.Fatalf("unsupported dialect %q", dialect)
	}
	require.NoError(t, err)
	if dialect != "sqlite" {
		if db.Migrator().HasTable("quota_mig_test") {
			t.Skipf("refusing to run quota BIGINT migration against external %s db: quota_mig_test table already exists", dialect)
		}
		t.Cleanup(func() {
			_ = db.Migrator().DropTable("quota_mig_test")
			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
		})
	}
	return db
}

func runQuotaBigIntMigrationTest(t *testing.T, dialect, dsn string) {
	t.Helper()
	db := openQuotaMigrationDB(t, dialect, dsn)

	// 1. 旧 schema（int32 列）+ 存量数据
	require.NoError(t, db.AutoMigrate(&legacyQuotaModel{}))
	legacy := legacyQuotaModel{Username: "legacy-user", Quota: 2147483647, UsedQuota: 100, AffQuota: 200, AffHistory: 300}
	require.NoError(t, db.Create(&legacy).Error)

	// 2. 新模型 AutoMigrate（int → int64 / type:bigint）
	require.NoError(t, db.AutoMigrate(&newQuotaModel{}))

	// 3. 数据保留：int32 最大值不截断
	var migrated newQuotaModel
	require.NoError(t, db.Where("username = ?", "legacy-user").First(&migrated).Error)
	require.Equal(t, int64(2147483647), migrated.Quota, "int32 边界值迁移后必须保留")
	require.Equal(t, int64(100), migrated.UsedQuota)
	require.Equal(t, int64(200), migrated.AffQuota)
	require.Equal(t, int64(300), migrated.AffHistory)

	// 4. 列类型断言：SQLite 为 bigint/integer（两者均为 64 位有符号），MySQL/PG 为 bigint
	if dialect == "sqlite" {
		colType := getSQLiteColumnType(t, db, "quota_mig_test", "quota")
		require.True(t, colType == "bigint" || colType == "integer",
			"sqlite quota column must be 64-bit (got %q)", colType)
	} else {
		colTypes, err := db.Migrator().ColumnTypes("quota_mig_test")
		require.NoError(t, err)
		found := false
		for _, ct := range colTypes {
			if ct.Name() == "quota" {
				// PostgreSQL/GORM 的 DatabaseTypeName() 返回内部类型名 int8（与 SQL 标准 bigint 是同一类型），
				// MySQL 返回 BIGINT。按 dialect 区分断言，避免迁移成功后误判失败。
				typeName := strings.ToUpper(ct.DatabaseTypeName())
				switch dialect {
				case "postgres":
					require.True(t, typeName == "INT8" || typeName == "BIGINT",
						"postgres quota column must be INT8/BIGINT (got %q)", ct.DatabaseTypeName())
				case "mysql":
					require.Equal(t, "BIGINT", typeName,
						"mysql quota column must be BIGINT (got %q)", ct.DatabaseTypeName())
				default:
					t.Fatalf("unsupported dialect %q", dialect)
				}
				found = true
			}
		}
		require.True(t, found, "quota column not found")
	}

	// 5. 新 int64 大值可写入（超出 int32 范围）
	big := newQuotaModel{Username: "big-user", Quota: 55555500000, UsedQuota: 0}
	require.NoError(t, db.Create(&big).Error)
	var readBack newQuotaModel
	require.NoError(t, db.Where("username = ?", "big-user").First(&readBack).Error)
	require.Equal(t, int64(55555500000), readBack.Quota)
}

func TestQuotaColumnsBigIntMigrationSQLite(t *testing.T) {
	runQuotaBigIntMigrationTest(t, "sqlite", "")
}

func TestQuotaColumnsBigIntMigrationMySQL(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set TEST_MYSQL_DSN to run mysql quota BIGINT migration test")
	}
	runQuotaBigIntMigrationTest(t, "mysql", dsn)
}

func TestQuotaColumnsBigIntMigrationPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run postgres quota BIGINT migration test")
	}
	runQuotaBigIntMigrationTest(t, "postgres", dsn)
}
