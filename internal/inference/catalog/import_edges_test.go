package catalog

import (
	"strings"
	"testing"
)

// import_edges_test.go — Task 16 覆盖率补强：LLM_PROVIDERS_JSON 解析的
// 校验分支（纯解析，不落库；幂等导入主路径已在 import_test.go）。

func TestParseEnvCatalog_ValidationBranches(t *testing.T) {
	// 非 JSON。
	if _, err := ParseEnvCatalog(`not json`); err == nil {
		t.Error("bad JSON accepted")
	}
	// 未知字段（DisallowUnknownFields）。
	if _, err := ParseEnvCatalog(`{"providers":[],"models":[],"mystery":1}`); err == nil {
		t.Error("unknown field accepted")
	}
	// 重复 provider code。
	dup := `{"providers":[
		{"code":"glm","display_name":"A","access_type":"official_api"},
		{"code":"glm","display_name":"B","access_type":"official_api"}],"models":[]}`
	if _, err := ParseEnvCatalog(dup); err == nil {
		t.Error("duplicate provider code accepted")
	}
	// 非法 provider（空 code）。
	badProv := `{"providers":[{"code":"","display_name":"A","access_type":"official_api"}],"models":[]}`
	if _, err := ParseEnvCatalog(badProv); err == nil {
		t.Error("invalid provider accepted")
	}
	// 模型引用不存在的 provider。
	ghostDep := `{"providers":[],"models":[{"id":"m1","display_name":"M","context_tokens":1000,
		"max_output_tokens":100,"protocols":["openai_chat"],
		"deployments":[{"provider":"ghost","upstream_model":"u","base_url":"https://a.example.com","protocol":"openai_chat"}]}]}`
	if _, err := ParseEnvCatalog(ghostDep); err == nil {
		t.Error("deployment referencing ghost provider accepted")
	}
	// 非法模型（bad id）。
	badModel := `{"providers":[],"models":[{"id":"Bad Model!","display_name":"M","context_tokens":1,
		"max_output_tokens":1,"protocols":["openai_chat"]}]}`
	if _, err := ParseEnvCatalog(badModel); err == nil {
		t.Error("invalid model accepted")
	}
	// 合法最小目录：modalities 默认 text。
	ok, err := ParseEnvCatalog(`{"providers":[{"code":"glm","display_name":"GLM","access_type":"official_api"}],
		"models":[{"id":"glm-1","display_name":"GLM 1","context_tokens":1000,"max_output_tokens":100,
		"protocols":["openai_chat"],
		"deployments":[{"provider":"glm","upstream_model":"u1","base_url":"https://api.glm.example.com","protocol":"openai_chat"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ok.Models) != 1 || len(ok.Models[0].InputModalities) != 1 || ok.Models[0].InputModalities[0] != "text" {
		t.Errorf("modalities default = %+v", ok.Models[0])
	}
	if !strings.Contains(ok.Providers[0].Code, "glm") {
		t.Errorf("provider = %+v", ok.Providers[0])
	}
}
