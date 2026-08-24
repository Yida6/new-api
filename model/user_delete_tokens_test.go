package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// createTestUserWithTokens 创建测试用户及其 API Key（令牌），返回用户与令牌快照。
func createTestUserWithTokens(t *testing.T, username string, tokenCount int) (*User, []Token) {
	t.Helper()
	u := &User{
		Username: username,
		Password: "test-password",
		Status:   common.UserStatusEnabled,
		Role:     common.RoleCommonUser,
		AffCode:  "aff-" + username,
	}
	require.NoError(t, DB.Create(u).Error)
	tokens := make([]Token, 0, tokenCount)
	for i := 0; i < tokenCount; i++ {
		tok := &Token{
			UserId: u.Id,
			Key:    fmt.Sprintf("sk-test-%s-%d", username, i),
			Name:   fmt.Sprintf("tok-%s-%d", username, i),
			Status: common.TokenStatusEnabled,
		}
		require.NoError(t, DB.Create(tok).Error)
		tokens = append(tokens, *tok)
	}
	return u, tokens
}

// TestUserDeleteSoftRemovesOwnTokens 用户主动注销（User.Delete）后：
// 该用户 token 被物理删除（Unscoped 也查不到），其他用户 token 不受影响。
func TestUserDeleteSoftRemovesOwnTokens(t *testing.T) {
	truncateTables(t)
	userA, tokensA := createTestUserWithTokens(t, "delete-self-a", 2)
	userB, tokensB := createTestUserWithTokens(t, "delete-self-b", 1)

	require.NoError(t, (&User{Id: userA.Id}).Delete())

	// 注销用户 token 已物理删除（含软删行）
	var leftA int64
	require.NoError(t, DB.Unscoped().Model(&Token{}).Where("user_id = ?", userA.Id).Count(&leftA).Error)
	assert.Equal(t, int64(0), leftA)
	_, err := GetTokenByKey(tokensA[0].Key, true)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)

	// 其他用户 token 不受影响
	var leftB int64
	require.NoError(t, DB.Unscoped().Model(&Token{}).Where("user_id = ?", userB.Id).Count(&leftB).Error)
	assert.Equal(t, int64(1), leftB)
	got, err := GetTokenByKey(tokensB[0].Key, true)
	require.NoError(t, err)
	assert.Equal(t, tokensB[0].Id, got.Id)

	// 注销用户被软删除：默认查询查不到，Unscoped 可查到且 deleted_at 非空
	var uA User
	assert.ErrorIs(t, DB.Where("id = ?", userA.Id).First(&uA).Error, gorm.ErrRecordNotFound)
	var uADeleted User
	require.NoError(t, DB.Unscoped().Where("id = ?", userA.Id).First(&uADeleted).Error)
	assert.False(t, uADeleted.DeletedAt.Time.IsZero())

	// 其他用户仍正常可查
	var uB User
	require.NoError(t, DB.Where("id = ?", userB.Id).First(&uB).Error)
	assert.Equal(t, userB.Id, uB.Id)
}

// TestUserDeleteRollsBackWhenTokenCleanupFails 令牌清理失败时，用户删除事务整体回滚：
// 用户不被软删、token 保留、auth_version 不变。
func TestUserDeleteRollsBackWhenTokenCleanupFails(t *testing.T) {
	truncateTables(t)
	user, _ := createTestUserWithTokens(t, "delete-self-rollback", 1)

	// 用 sqlite 触发器强制 tokens 删除失败，模拟事务中途错误
	require.NoError(t, DB.Exec(
		`CREATE TRIGGER fail_token_delete BEFORE DELETE ON tokens
		 BEGIN SELECT RAISE(ABORT, 'forced token delete failure'); END;`,
	).Error)
	t.Cleanup(func() {
		require.NoError(t, DB.Exec(`DROP TRIGGER IF EXISTS fail_token_delete`).Error)
	})

	err := (&User{Id: user.Id}).Delete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced token delete failure")

	// 用户未被软删（默认查询仍可查到）
	var u User
	require.NoError(t, DB.Where("id = ?", user.Id).First(&u).Error)
	assert.Equal(t, user.Id, u.Id)

	// token 未被删除
	var cnt int64
	require.NoError(t, DB.Unscoped().Model(&Token{}).Where("user_id = ?", user.Id).Count(&cnt).Error)
	assert.Equal(t, int64(1), cnt)

	// auth_version 回滚到原值（证明整个事务已回滚）
	var uUnscoped User
	require.NoError(t, DB.Unscoped().Where("id = ?", user.Id).First(&uUnscoped).Error)
	assert.Equal(t, user.AuthVersion, uUnscoped.AuthVersion)
}

// TestUserHardDeleteStillRemovesTokens 管理员硬删除路径行为保持不变：
// token 与用户均被物理删除。
func TestUserHardDeleteStillRemovesTokens(t *testing.T) {
	truncateTables(t)
	user, _ := createTestUserWithTokens(t, "hard-delete", 2)

	require.NoError(t, (&User{Id: user.Id}).HardDelete())

	var cnt int64
	require.NoError(t, DB.Unscoped().Model(&Token{}).Where("user_id = ?", user.Id).Count(&cnt).Error)
	assert.Equal(t, int64(0), cnt)

	var u User
	assert.ErrorIs(t, DB.Unscoped().Where("id = ?", user.Id).First(&u).Error, gorm.ErrRecordNotFound)
}
