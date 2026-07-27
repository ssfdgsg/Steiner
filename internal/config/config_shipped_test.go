package config

import (
	"path/filepath"
	"testing"
)

// TestLoadShippedConfigs 仓库内置的两份配置必须始终可加载通过校验，
// 防止新增配置段（如 cluster）后示例文件与校验逻辑脱节。
func TestLoadShippedConfigs(t *testing.T) {
	for _, name := range []string{"gateway.yaml", "gateway.example.yaml"} {
		path := filepath.Join("..", "..", "configs", name)
		if _, err := Load(path); err != nil {
			t.Errorf("加载 %s 失败: %v", name, err)
		}
	}
}
