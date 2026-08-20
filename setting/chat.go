package setting

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/common"
)

// Chats 默认值为空:线上中转站不暴露聊天客户端跳转入口。
// 历史默认(2026-08-20 前)包含 Cherry Studio / AionUI / 流畅阅读 / CC Switch /
// DeepChat / Lobe Chat / AI as Workspace / AMA 问天 / OpenCat 共 9 项,如有
// 回滚需要可参考 git history。
var Chats = []map[string]string{}

func UpdateChatsByJsonString(jsonString string) error {
	Chats = make([]map[string]string, 0)
	return json.Unmarshal([]byte(jsonString), &Chats)
}

func Chats2JsonString() string {
	jsonBytes, err := json.Marshal(Chats)
	if err != nil {
		common.SysLog("error marshalling chats: " + err.Error())
		return "[]"
	}
	return string(jsonBytes)
}
