// paths.go 由 state.json 路径推导同目录的配套持久化文件。
//
// 三个文件都是「数据目录里的伴生物」，跟 state.json 一起走（Docker ./data volume、
// 整体迁移时的 auths + data 目录）。state 路径为空（纯内存测试形态）→ 返回空 =
// 对应文件不落盘。
package gateway

import "path/filepath"

// ModelJSONPath 由 state.json 推导 model.json（context_length 四级查找链第 3 级缓存）。
func ModelJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "model.json")
}

// OutputJSONPath 由 state.json 推导 output.json（输出侧运行期可调项：格式/前缀/费率/出口）。
func OutputJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "output.json")
}

// ZCodeEntitledJSONPath 由 state.json 推导 zcode_entitled.json（zcode 套餐额度收敛快照）。
func ZCodeEntitledJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "zcode_entitled.json")
}
