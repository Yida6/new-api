package controller

import (
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// setupTokenQuotaBalanceTestUser 创建带指定钱包余额的用户，返回其信息。
func setupTokenQuotaBalanceTestUser(t *testing.T, id int, quota int64) *model.User {
	t.Helper()
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.User{}))
	user := &model.User{
		Id:       id,
		Username: "quota-balance-user",
		Password: "password",
		Group:    "default",
		Status:   common.UserStatusEnabled,
		Quota:    quota,
	}
	require.NoError(t, db.Create(user).Error)
	return user
}

// quotaTokenRequest 构造带指定额度/无限标志的建 key 请求体。
func quotaTokenRequest(name string, remainQuota int, unlimited bool) map[string]any {
	return map[string]any{
		"name":            name,
		"expired_time":    -1,
		"remain_quota":    remainQuota,
		"unlimited_quota": unlimited,
		"group":           "default",
	}
}

// setContextRole 在 ctx 上设置角色（newAuthenticatedContext 默认不设置，GetInt 返回 0=GuestUser）。
func setContextRole(ctx *gin.Context, role int) {
	ctx.Set("role", role)
}

func TestAddTokenQuotaExceedsBalanceRejected(t *testing.T) {
	user := setupTokenQuotaBalanceTestUser(t, 201, 100)
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", quotaTokenRequest("over-balance", 200, false), user.Id)
	AddToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.False(t, response.Success, "expected rejection when remain_quota > wallet balance")
	require.True(t, strings.Contains(response.Message, "钱包余额"), "message should mention wallet balance, got: %s", response.Message)

	var count int64
	require.NoError(t, model.DB.Model(&model.Token{}).Count(&count).Error)
	require.Equal(t, int64(0), count, "no token should be inserted on rejection")
}

func TestAddTokenQuotaWithinBalanceAllowed(t *testing.T) {
	user := setupTokenQuotaBalanceTestUser(t, 202, 100)
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", quotaTokenRequest("within-balance", 50, false), user.Id)
	AddToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
}

func TestAddTokenQuotaEqualBalanceAllowed(t *testing.T) {
	user := setupTokenQuotaBalanceTestUser(t, 203, 100)
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", quotaTokenRequest("equal-balance", 100, false), user.Id)
	AddToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
}

func TestAddTokenUnlimitedQuotaBypassesBalance(t *testing.T) {
	user := setupTokenQuotaBalanceTestUser(t, 204, 0)
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", quotaTokenRequest("unlimited", 1000, true), user.Id)
	AddToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
}

func TestAddTokenAdminBypassesBalanceCheck(t *testing.T) {
	user := setupTokenQuotaBalanceTestUser(t, 205, 0)
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", quotaTokenRequest("admin-token", 200, false), user.Id)
	setContextRole(ctx, common.RoleAdminUser)
	AddToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
}

func TestUpdateTokenQuotaExceedsBalanceRejected(t *testing.T) {
	user := setupTokenQuotaBalanceTestUser(t, 206, 100)
	token := seedToken(t, model.DB, user.Id, "editable", "balance1234update5678")

	body := map[string]any{
		"id":              token.Id,
		"name":            "updated",
		"expired_time":    -1,
		"remain_quota":    200,
		"unlimited_quota": false,
		"group":           "default",
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/token/", body, user.Id)
	UpdateToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.False(t, response.Success, "expected rejection when updating remain_quota above balance")
	require.True(t, strings.Contains(response.Message, "钱包余额"), "message should mention wallet balance, got: %s", response.Message)
}

func TestUpdateTokenStatusOnlySkipsBalanceCheck(t *testing.T) {
	user := setupTokenQuotaBalanceTestUser(t, 207, 0)
	token := seedToken(t, model.DB, user.Id, "status-token", "status1234toggle5678")

	// status_only 模式只改状态，不改额度：即使余额为 0 且请求携带超额 remain_quota，也应放行。
	body := map[string]any{
		"id":              token.Id,
		"name":            "status-token",
		"expired_time":    -1,
		"remain_quota":    200,
		"unlimited_quota": false,
		"group":           "default",
		"status":          common.TokenStatusDisabled,
	}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/token/?status_only=1", body, user.Id)
	UpdateToken(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)
}
