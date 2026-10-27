package source

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// —— 配置导出 / 导入 ——

// ExportFile 导出文件的完整结构。version 便于未来格式演进时做迁移。
// 导入端兼容三种来源：本结构导出的文件、裸数组（stored 列表）、sources.json 本身。
type ExportFile struct {
	Version  int       `json:"version"` // 当前固定为 1
	Exported time.Time `json:"exported_at"`
	Redacted bool      `json:"credentials_redacted"` // true = 凭据已脱敏，导入前必须补填
	Sources  []stored  `json:"sources"`
}

// Export 导出全部源定义。redact=true 时凭据字段替换为 "******"
// （用于把配置给他人看/入版本库的模板场景）；redact=false 的导出文件
// 含明文凭据，调用方必须提示用户妥善保管。
func (m *Manager) Export(redact bool) *ExportFile {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]stored, 0, len(m.order))
	for _, id := range m.order {
		s := m.sources[id]
		st := stored{ID: s.ID, Name: s.Name, Type: s.Type, Config: map[string]any{}}
		for k, v := range s.Config {
			st.Config[k] = v
		}
		if redact {
			for k, v := range st.Config {
				if isSecretKey(k) && v != "" {
					st.Config[k] = redactedMark
				}
			}
		}
		list = append(list, st)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return &ExportFile{Version: 1, Exported: time.Now(), Redacted: redact, Sources: list}
}

const redactedMark = "******"

// ImportItem 单条源的导入结果。
type ImportItem struct {
	ID     string `json:"id"`
	Status string `json:"status"` // added / updated / skipped / failed
	Error  string `json:"error,omitempty"`
}

// ImportReport 整体导入报告。
type ImportReport struct {
	Total    int          `json:"total"`
	Added    int          `json:"added"`
	Updated  int          `json:"updated"`
	Skipped  int          `json:"skipped"`
	Failed   int          `json:"failed"`
	Items    []ImportItem `json:"items"`
	Redacted int          `json:"redacted_count"` // 被识别为已脱敏凭据的条数（提醒补填）
}

// ImportRaw 接受任意 JSON 载荷并解析为源列表，兼容：
//   - ExportFile（{"version":1,"sources":[...]}）
//   - 裸数组（[{"id":...,"type":...,"config":{...}}]）
func (m *Manager) ImportRaw(data []byte, overwrite bool) (*ImportReport, error) {
	trimmed := trimJSONSpace(data)
	var entries []stored
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var f ExportFile
		if err := json.Unmarshal(trimmed, &f); err != nil {
			return nil, fmt.Errorf("解析导出文件失败: %w", err)
		}
		entries = f.Sources
	} else {
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("解析失败：既不是导出文件也不是源数组: %w", err)
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("导入文件中没有源定义")
	}
	return m.Import(entries, overwrite), nil
}

// Import 批量导入：逐条独立处理，单条失败不影响其余条目。
// overwrite=false 时已存在的 ID 记为 skipped。
// 每条成功即持久化一次（与页面逐条新增语义一致）。
func (m *Manager) Import(entries []stored, overwrite bool) *ImportReport {
	rep := &ImportReport{Total: len(entries), Items: make([]ImportItem, 0, len(entries))}
	for _, e := range entries {
		item := ImportItem{ID: e.ID}
		if _, err := m.Get(e.ID); err == nil {
			if !overwrite {
				rep.Skipped++
				item.Status = "skipped"
				item.Error = "已存在同名 ID（可勾选“覆盖已有源”后重试）"
				rep.Items = append(rep.Items, item)
				continue
			}
			if err := m.Remove(e.ID); err != nil {
				rep.Failed++
				item.Status = "failed"
				item.Error = "移除旧定义失败: " + err.Error()
				rep.Items = append(rep.Items, item)
				continue
			}
			if _, err := m.Add(e.ID, e.Name, e.Type, e.Config); err != nil {
				rep.Failed++
				item.Status = "failed"
				item.Error = "覆盖写入失败: " + err.Error()
				rep.Items = append(rep.Items, item)
				continue
			}
			rep.Updated++
			item.Status = "updated"
			if countRedacted(e.Config) > 0 {
				rep.Redacted++
			}
			rep.Items = append(rep.Items, item)
			continue
		}
		if _, err := m.Add(e.ID, e.Name, e.Type, e.Config); err != nil {
			rep.Failed++
			item.Status = "failed"
			item.Error = err.Error()
			rep.Items = append(rep.Items, item)
			continue
		}
		rep.Added++
		item.Status = "added"
		if countRedacted(e.Config) > 0 {
			rep.Redacted++
		}
		rep.Items = append(rep.Items, item)
	}
	return rep
}

func countRedacted(cfg map[string]any) int {
	n := 0
	for k, v := range cfg {
		if isSecretKey(k) && v == redactedMark {
			n++
		}
	}
	return n
}

func trimJSONSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}
